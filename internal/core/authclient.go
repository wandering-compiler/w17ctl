package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
	authpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/auth"
)

// AuthOrg is one org-membership row in the CLI's neutral shape (decoupled from
// the pb type so cmd packages don't all import authpb).
type AuthOrg struct {
	ID   string
	Slug string
	Name string
	Kind string // private | company
	Role string // owner | admin | member
}

// SignedIn is what a successful sign-in hands the client to store.
type SignedIn struct {
	Token  string
	UserID string
	// ExpiresAt is when the console says the session ends; zero = the
	// response did not say. See signInExpiry.
	ExpiresAt time.Time
	// Device is the thumbprint of the device key the console bound Token to,
	// "" when it bound none (a console whose auth binds no devices). Stored
	// beside the token: it says the token is presented with a DPoP proof.
	Device string
}

// SignIn authenticates email+password against the console auth gateway at addr
// and returns the minted bearer + the principal's user id. The RPC is
// unauthenticated (it is the credential that mints the bearer). A wrong
// email/password surfaces as a gRPC Unauthenticated error (anti-enumeration —
// the server maps every failure to one opaque status).
//
// The call carries a DPoP proof signed by the console's device key (made here
// on first use): a console whose auth binds devices binds the new token to that key.
// Such a console verified the proof, and a verified proof is answered with a
// `dpop-nonce` for the next one — that header is how SignIn knows the token is
// bound. A console that binds nothing ignores the proof and sends no nonce,
// and its token stays a plain bearer. Nothing is guessed from the address or
// the version: the answer comes from the console that minted the token.
//
// DEVICE_REVOKED on sign-in means the key itself was revoked (a revoked device
// is refused for good): the key is discarded and SignIn retried once with a
// new one, which the console sees as a new device. A key named by
// W17_DPOP_KEY is the operator's and is not replaced.
func SignIn(ctx context.Context, addr, email, password string) (SignedIn, error) {
	signed, err := signInOnce(ctx, addr, email, password)
	if err == nil {
		return signed, nil
	}
	if hint := htuMismatchHint(addr, err); hint != "" {
		return SignedIn{}, fmt.Errorf("%w\n%s", err, hint)
	}
	if !errors.Is(err, dpopclient.ErrDeviceRevoked) {
		return SignedIn{}, err
	}
	if _, explicit, _ := DeviceKeyPath(addr); explicit {
		return SignedIn{}, fmt.Errorf("%w\n  the key %s names was revoked on this console; point it at a new key (w17ctl key public makes one)", err, EnvDPoPKeyVar)
	}
	// The refusal already discarded the key (consolePerRPC.noteRevoked); a
	// new one is made on the retry.
	return signInOnce(ctx, addr, email, password)
}

func signInOnce(ctx context.Context, addr, email, password string) (SignedIn, error) {
	key, err := EnsureDeviceKey(addr)
	if err != nil {
		return SignedIn{}, err
	}
	cl, conn, err := DialAuthService(addr)
	if err != nil {
		return SignedIn{}, fmt.Errorf("dial console: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var hdr metadata.MD
	resp, err := cl.SignIn(dpopclient.MintContext(ctx), &authpb.SignInReq{Email: email, Password: password}, grpc.Header(&hdr))
	if err != nil {
		return SignedIn{}, err
	}
	if resp.GetToken() == "" {
		return SignedIn{}, fmt.Errorf("invalid email or password")
	}
	signed := SignedIn{Token: resp.GetToken(), UserID: resp.GetUserId(), ExpiresAt: signInExpiry(resp)}
	if len(hdr.Get(dpop.MetadataNonce)) > 0 {
		signed.Device = key.Thumbprint()
	}
	return signed, nil
}

// signInExpiry is the one place a sign-in response's session expiry is read.
//
// The console's SignInResp carries none today, so the answer is the zero time
// (unknown) and login records no expiry. The client still learns one: an
// expired session's refusal names its expiry, and ExplainAuthFailure writes it
// to the store. When the response grows the field, it is read here and nothing
// downstream changes.
func signInExpiry(*authpb.SignInResp) time.Time { return time.Time{} }

// ListMyOrgs lists the organizations the bearer belongs to, dialing the console
// auth gateway at addr and presenting token as the request bearer — with a
// DPoP proof when the token is bound to the device key (the stored login's
// Device, or W17_TOKEN with W17_DPOP_KEY). A nil/empty result is not an error
// (a fresh account may belong to no org yet).
func ListMyOrgs(ctx context.Context, addr, token string) ([]AuthOrg, error) {
	return listMyOrgs(ctx, addr, token, bindingFor(addr, token))
}

// ListMyOrgsAs is ListMyOrgs for a token that is not stored yet — login's,
// right after SignIn: device is the key thumbprint the token is bound to
// (SignedIn.Device), "" for a plain bearer.
func ListMyOrgsAs(ctx context.Context, addr, token, device string) ([]AuthOrg, error) {
	return listMyOrgs(ctx, addr, token, tokenBinding{bound: device != "", device: device})
}

func listMyOrgs(ctx context.Context, addr, token string, b tokenBinding) ([]AuthOrg, error) {
	return listMyOrgsIn(ctx, addr, token, b, "")
}

// listMyOrgsIn is listMyOrgs sent as acting in orgSlug ("" = none): the form
// an api-realm token needs, whose ListMyOrgs grant is itself org-scoped
// (listOrgsOnce).
func listMyOrgsIn(ctx context.Context, addr, token string, b tokenBinding, orgSlug string) ([]AuthOrg, error) {
	cl, conn, err := DialAuthService(addr)
	if err != nil {
		return nil, fmt.Errorf("dial console: %w", err)
	}
	defer func() { _ = conn.Close() }()
	authCtx := withCallToken(ctx, token, b)
	if orgSlug != "" {
		authCtx = metadata.AppendToOutgoingContext(authCtx, "w17-org", orgSlug)
	}
	resp, err := cl.ListMyOrgs(authCtx, &authpb.ListMyOrgsReq{})
	if err != nil {
		return nil, err
	}
	orgs := resp.GetOrgs()
	out := make([]AuthOrg, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, AuthOrg{
			ID:   o.GetOrgId(),
			Slug: o.GetSlug(),
			Name: o.GetName(),
			Kind: o.GetKind(),
			Role: o.GetRole(),
		})
	}
	return out, nil
}
