package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
	authpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/auth"
	consolerpcpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/rpc"
)

// consolePerRPC is the console credential: the token for the console being
// dialed, and — on the dials that act in an organization — the active-org
// header every scoped call needs.
//
// The token goes out in one of two forms:
//
//   - `authorization: Bearer <token>`, for a token the console did not bind
//     to a key — every token from a console whose auth binds no devices;
//   - `authorization: DPoP <token>` plus a `dpop` proof signed by the
//     console's device key (dpopclient), for a token bound to it at sign-in
//     (the stored instance's Device), or for W17_TOKEN when W17_DPOP_KEY is
//     set.
//
// The choice is per token rather than per console guess: a console that binds
// devices refuses a bound token sent as Bearer, and a console that does not
// refuses the DPoP scheme. Sign-in learns which one it talked to (SignIn), so
// the stored record says how to present what it holds.
//
// A call made with dpopclient.MintContext and no token carries a proof alone:
// sign-in, which binds the new token to the proof's key.
//
// With no token and no mint mark it adds nothing — identical to the pre-auth
// behaviour, so it is safe to wire onto every dial.
//
// RequireTransportSecurity is false, as it always was: tests dial in-process
// listeners in clear text, and every real console dial is TLS
// (ConsoleTransportCreds), so the token rides the encrypted channel.
type consolePerRPC struct {
	addr string
	// stored: resolve the token from W17_TOKEN / the credential store and send
	// the active org. The auth dial sets neither — login must work before any
	// token is stored — and passes a token per call (withCallToken).
	stored bool
	origin string
}

func newConsolePerRPC(addr string, stored bool) *consolePerRPC {
	return &consolePerRPC{addr: addr, stored: stored, origin: DPoPOrigin(addr)}
}

// ConsolePerRPC is the console credential for addr, for code that inspects
// what a call would carry. Dials install ConsoleCredentials, which also brings
// the nonce and refusal handling.
func ConsolePerRPC(addr string) credentials.PerRPCCredentials { return newConsolePerRPC(addr, true) }

// ConsoleCredentials returns the dial options that install the console
// credential for addr: the per-call credential, the DPoP nonce handling (the
// console's nonce is learned and a stale-nonce refusal retried once), and the
// device-revocation handling. The storage-tier dials use THIS one rather than
// a copy — a copy is how the org header once went missing from half the CLI.
func ConsoleCredentials(addr string) []grpc.DialOption {
	return newConsolePerRPC(addr, true).dialOptions()
}

func (c *consolePerRPC) dialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(c),
		grpc.WithChainUnaryInterceptor(c.unary),
		grpc.WithChainStreamInterceptor(c.stream),
	}
}

// ---- nonces, shared by the process ---------------------------------------

// dpopEntry is the dpopclient credential for one (origin, key): it holds the
// last nonce that console sent for that key. It is shared by every connection
// the process opens to the console, because each command dials afresh and a
// per-connection credential starts every connection without a nonce — which
// the console answers with a challenge. A unary call recovers (dpopclient
// retries once); a stream cannot, so its first message would fail.
type dpopEntry struct {
	dc *dpopclient.Credentials

	mu      sync.Mutex
	learned time.Time // when a response last carried a nonce

	// probeMu makes the pre-stream nonce probe single-flight: streams opened
	// together wait for one probe instead of each sending their own.
	probeMu sync.Mutex
}

// nonceFresh is how long a learned nonce is trusted to still be current. The
// console's nonces live two buckets (dpop.DefaultNonceBucket each), so one
// learned within a bucket is valid for at least another bucket; four fifths of
// a bucket leaves room for clock skew and a slow call.
const nonceFresh = dpop.DefaultNonceBucket * 4 / 5

var (
	dpopEntriesMu sync.Mutex
	dpopEntries   = map[string]*dpopEntry{}
	nonceClock    = time.Now
)

func dpopEntryFor(origin string, key *dpop.Key) *dpopEntry {
	id := origin + "|" + key.Thumbprint()
	dpopEntriesMu.Lock()
	defer dpopEntriesMu.Unlock()
	e := dpopEntries[id]
	if e == nil {
		e = &dpopEntry{dc: &dpopclient.Credentials{
			Signer:        key,
			Origin:        origin,
			Token:         func(ctx context.Context) (string, error) { return callTokenFrom(ctx).token, nil },
			AllowInsecure: true,
		}}
		dpopEntries[id] = e
	}
	return e
}

func resetDPoPCreds() {
	dpopEntriesMu.Lock()
	dpopEntries = map[string]*dpopEntry{}
	dpopEntriesMu.Unlock()
}

func (e *dpopEntry) touch() {
	e.mu.Lock()
	e.learned = nonceClock()
	e.mu.Unlock()
}

func (e *dpopEntry) stale() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.learned.IsZero() || nonceClock().Sub(e.learned) > nonceFresh
}

// ---- what a call carries -------------------------------------------------

// callToken is a token handed to one call rather than resolved from the store
// — the auth dial's ListMyOrgs during login, before anything is stored.
type callToken struct {
	token string
	b     tokenBinding
}

type callTokenKey struct{}

func withCallToken(ctx context.Context, token string, b tokenBinding) context.Context {
	return context.WithValue(ctx, callTokenKey{}, callToken{token: token, b: b})
}

func callTokenFrom(ctx context.Context) callToken {
	ct, _ := ctx.Value(callTokenKey{}).(callToken)
	return ct
}

// tokenBinding says how a token is presented: bound (DPoP + proof) or not
// (Bearer). device is the thumbprint the token is bound to; "" on a bound
// token means "whatever key W17_DPOP_KEY holds" — an environment token, whose
// binding w17ctl never saw.
type tokenBinding struct {
	bound  bool
	device string
}

// bindingFor resolves how the token for addr is presented: W17_TOKEN is bound
// when W17_DPOP_KEY is set; a stored token is bound when login recorded the
// key the console bound it to.
func bindingFor(addr, token string) tokenBinding {
	if token == "" {
		return tokenBinding{}
	}
	if env := os.Getenv(EnvTokenVar); env != "" && env == token {
		return tokenBinding{bound: os.Getenv(EnvDPoPKeyVar) != ""}
	}
	if inst := instanceFor(addr); inst != nil && inst.Token == token && inst.Device != "" {
		return tokenBinding{bound: true, device: inst.Device}
	}
	return tokenBinding{}
}

// callPlan is what one call carries: a token (or none), and the key that signs
// its proof (nil = no proof). mint is a sign-in's proof with no token.
type callPlan struct {
	token string
	b     tokenBinding
	key   *dpop.Key
	mint  bool
}

func (c *consolePerRPC) plan(ctx context.Context) (callPlan, error) {
	ct := callTokenFrom(ctx)
	if c.stored && ct.token == "" {
		ct.token = AuthTokenFn(c.addr)
		ct.b = bindingFor(c.addr, ct.token)
	}
	p := callPlan{token: ct.token, b: ct.b}
	switch {
	case ct.token == "" && dpopclient.IsMint(ctx):
		// Sign-in: a proof alone. SignIn made or loaded the key.
		k, err := LoadDeviceKey(c.addr)
		if err != nil {
			return p, err
		}
		p.key, p.mint = k, true
	case ct.b.bound:
		k, err := c.boundKey(ct.b)
		if err != nil {
			return p, err
		}
		p.key = k
	}
	return p, nil
}

// boundKey loads the device key a bound token needs, and refuses one that is
// not the key the token was bound to: the console would refuse the proof
// anyway, and only this side can say why.
func (c *consolePerRPC) boundKey(b tokenBinding) (*dpop.Key, error) {
	k, err := LoadDeviceKey(c.addr)
	if err != nil {
		return nil, fmt.Errorf("%w — the session for %s is bound to this machine's device key, so it cannot be used without it\n"+
			"  fix: w17ctl login %s (a new key is made)", err, displayAddr(c.addr), displayAddr(c.addr))
	}
	if b.device != "" && k.Thumbprint() != b.device {
		// The cached key may be stale: another shell can have replaced the
		// file (a login after a revocation) and stored the session bound to
		// the new key. Read the file once more before refusing.
		if k, err = reloadDeviceKey(c.addr); err == nil && k.Thumbprint() == b.device {
			return k, nil
		}
		path, _, _ := DeviceKeyPath(c.addr)
		return nil, fmt.Errorf("the device key at %s is not the one the session for %s is bound to\n"+
			"  fix: w17ctl login %s", path, displayAddr(c.addr), displayAddr(c.addr))
	}
	return k, nil
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c *consolePerRPC) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	p, err := c.plan(ctx)
	if err != nil {
		return nil, err
	}
	var md map[string]string
	switch {
	case p.key != nil:
		md, err = dpopEntryFor(c.origin, p.key).dc.GetRequestMetadata(withCallToken(ctx, p.token, p.b), uri...)
		if err != nil {
			return nil, err
		}
		if p.mint {
			return md, nil
		}
	case p.token == "":
		return nil, nil
	default:
		md = map[string]string{"authorization": "Bearer " + p.token}
	}
	notePresentedToken(c.addr, p.token)
	if c.stored {
		// The ACTIVE org rides every console call, not just `init`.
		//
		// The console stamps scopes["org_id"] from this header after
		// validating membership, and a scoped model refuses a request that
		// carries no scope. Server-side there is an inference for the
		// single-org case, so a one-org user never noticed — but a user in TWO
		// orgs got a refusal from every scoped command, because `init` was the
		// only place that ever sent the header.
		//
		// It belongs here rather than at each call site for the reason the
		// token does: a per-call decision is one somebody forgets.
		//
		// A call that names its organization itself (`project register
		// --org`, `init`) is left alone: the explicit choice is the answer,
		// and adding the resolved one sent the header twice.
		if !outgoingNamesOrg(ctx) {
			slug, err := OrgSlugForRequestFn(c.addr)
			if err != nil {
				return nil, err
			}
			if slug != "" {
				md["w17-org"] = slug
			}
		}
	}
	return md, nil
}

func outgoingNamesOrg(ctx context.Context) bool {
	md, _ := metadata.FromOutgoingContext(ctx)
	return len(md.Get("w17-org")) > 0
}

// RequireTransportSecurity implements credentials.PerRPCCredentials.
func (*consolePerRPC) RequireTransportSecurity() bool { return false }

// ---- interceptors --------------------------------------------------------

// unary runs a signed call through the (origin, key) credential's
// interceptor — it learns the nonce and retries a stale-nonce refusal once —
// and discards the key on DEVICE_REVOKED. A call that carries no proof (or
// whose plan failed: GetRequestMetadata reports that) goes straight through.
func (c *consolePerRPC) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	p, err := c.plan(ctx)
	if err != nil || p.key == nil {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	e := dpopEntryFor(c.origin, p.key)
	// Fresh only when the console actually SENT a nonce: a call that never
	// reached it (Unavailable, a deadline) learned nothing. On a retry the
	// options are filled again from the second response.
	var hdr, trl metadata.MD
	opts = append(opts[:len(opts):len(opts)], grpc.Header(&hdr), grpc.Trailer(&trl))
	err = e.dc.UnaryClientInterceptor()(ctx, method, req, reply, cc, invoker, opts...)
	if len(hdr.Get(dpop.MetadataNonce)) > 0 || len(trl.Get(dpop.MetadataNonce)) > 0 {
		e.touch()
	}
	c.noteRevoked(err, p.key)
	return err
}

// stream is unary's counterpart, with one addition. dpopclient cannot retry a
// stream refused for a stale nonce — the refusal arrives after the caller may
// have sent — so a stream opened while this process holds no fresh nonce for
// the console would fail on its first receive (`w17ctl guide`, `client
// generate`: their first console call is a stream). One cheap signed unary
// call first (ListMyOrgs, which every session may make) learns the nonce; its
// outcome is ignored, since even its refusal carries one.
func (c *consolePerRPC) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	p, err := c.plan(ctx)
	if err != nil || p.key == nil {
		return streamer(ctx, desc, cc, method, opts...)
	}
	e := dpopEntryFor(c.origin, p.key)
	if !p.mint && e.stale() {
		e.probeMu.Lock()
		if e.stale() {
			probe := context.WithValue(ctx, unrecordedCallKey{}, true)
			_ = cc.Invoke(probe, consolerpcpb.AuthService_ListMyOrgs_FullMethodName, &authpb.ListMyOrgsReq{}, &authpb.ListMyOrgsResp{})
		}
		e.probeMu.Unlock()
	}
	s, err := e.dc.StreamClientInterceptor()(ctx, desc, cc, method, streamer, opts...)
	if err != nil {
		c.noteRevoked(err, p.key)
		return nil, err
	}
	return revokedClientStream{ClientStream: s, c: c, key: p.key}, nil
}

type revokedClientStream struct {
	grpc.ClientStream
	c   *consolePerRPC
	key *dpop.Key
}

func (s revokedClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	s.c.noteRevoked(err, s.key)
	return err
}

// noteRevoked discards the console's device key when it answered
// DEVICE_REVOKED: a revoked device is refused for good, so the key is no use
// to anyone, and the next login makes a new one. The refusal itself reaches
// the caller unchanged; ExplainAuthFailure says what to do.
func (c *consolePerRPC) noteRevoked(err error, key *dpop.Key) {
	if err != nil && errors.Is(err, dpopclient.ErrDeviceRevoked) {
		discardRevokedDeviceKey(c.addr, key.Thumbprint())
	}
}
