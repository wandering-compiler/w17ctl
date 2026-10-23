package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/oauthas"
)

// The browser login: w17ctl as a public OAuth 2.1 client (RFC 8252, a native
// app) of the console's authorization server — the auth plugin's
// `oauth_server` feature, wire in the SDK's lib/oauthas.
//
// The flow is the standard one and nothing in it is w17-specific: metadata
// (RFC 8414) → a client registered once per issuer (RFC 7591) → an
// authorization request with PKCE (S256) and a loopback redirect on a random
// port → the user signs in and approves on the console's consent page → the
// code is exchanged at the token endpoint with a DPoP proof (RFC 9449) of this
// machine's device key for the console. The request names no `resource`, and
// with a loopback redirect that is what makes the server grant a SESSION: the
// access token is an ordinary console session bound to the device key, the
// same thing the password sign-in stores.

// EnvOAuthIssuerVar names the authorization server's issuer when it is not
// the one derived from the console address (OAuthIssuer).
const EnvOAuthIssuerVar = "W17_CONSOLE_OAUTH_ISSUER"

// OAuthIssuer is the issuer w17ctl signs in through for the console at addr:
// explicit (the --issuer flag) when given, else W17_CONSOLE_OAUTH_ISSUER, else
// derived from addr — the console's host over https with the gRPC port
// dropped: grpcs://api.w17.app:50051 → https://api.w17.app. The console
// serves gRPC on its own port and its REST edge, where the OAuth endpoints
// live at the root, on 443 of the same host. A console whose REST edge is
// elsewhere (a local dev stack publishes it on another port) needs the
// override.
func OAuthIssuer(explicit, addr string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv(EnvOAuthIssuerVar)); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	u, err := url.Parse(dialOrigin(addr))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host
}

// OAuthLogin is one browser sign-in.
type OAuthLogin struct {
	// Addr is the console address: it selects the device key the session is
	// bound to, and is what the caller stores the session under.
	Addr string
	// Issuer is the authorization server's issuer (OAuthIssuer).
	Issuer string
	// ClientID is the client registered with Issuer earlier, "" for none.
	ClientID string
	// SaveClientID keeps a newly registered client id for the next login
	// (nil = not kept). Called as soon as the server issues one, so an
	// abandoned login does not register again next time.
	SaveClientID func(clientID string) error
	// Out is where the URL and progress are printed; nil = Stdout.
	Out io.Writer
	// Wait bounds how long the user has to finish in the browser; 0 =
	// OAuthCallbackWait.
	Wait time.Duration
	// ReadCode, when set, makes this the sign-in with a code, for a w17ctl
	// on another machine than the browser (an ssh session, a remote
	// workspace), which a redirect to this machine cannot reach: the browser
	// ends on the console's code page (oauthas.CodePath) and the person types
	// the `code#state` it shows; ReadCode asks for it. nil = the browser on
	// this machine, answering on a loopback redirect.
	ReadCode func() (string, error)
}

// OAuthCallbackWait is how long a browser login waits for the user.
const OAuthCallbackWait = 5 * time.Minute

// oauthHTTPTimeout bounds each request to the authorization server.
const oauthHTTPTimeout = 30 * time.Second

// OpenBrowserFn opens a URL in the user's browser, best effort. Tests replace
// it with a func that plays the user's part.
var OpenBrowserFn = func(string) error { return errors.New("no browser opener") }

// ErrOAuthUnavailable is a console that does not serve the browser login: no
// authorization server, or one that cannot issue w17ctl a session.
var ErrOAuthUnavailable = errors.New("the console does not offer browser sign-in")

// ErrOAuthDenied is the user declining the sign-in on the consent page.
var ErrOAuthDenied = errors.New("the sign-in was declined in the browser")

// oauthMetadata is the part of the RFC 8414 document w17ctl uses.
type oauthMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	DPoPAlgs              []string `json:"dpop_signing_alg_values_supported"`
	// IssParameter: the server sends `iss` with every authorization
	// response (RFC 9207), so one without it is refused.
	IssParameter bool `json:"authorization_response_iss_parameter_supported"`
}

// oauthErrorBody is an OAuth error response (RFC 6749 §5.2).
type oauthErrorBody struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e oauthErrorBody) String() string {
	if e.Description == "" {
		return e.Code
	}
	return e.Code + ": " + e.Description
}

// oauthHTTPClient talks to the authorization server: the console's TLS trust
// (a loopback issuer also trusts the dev CA), and no redirects followed — an
// endpoint that redirects is not one this client was told about.
var oauthHTTPClient = func(issuer string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = consoleTLSConfig(os.Getenv, issuerHostPort(issuer))
	return &http.Client{
		Transport:     tr,
		Timeout:       oauthHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func issuerHostPort(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil {
		return issuer
	}
	return u.Host
}

// SignInWithBrowser runs the browser login and returns the session to store,
// like SignIn does for a password. UserID is "": the token response names no
// user, and the identity is the console's to know.
func SignInWithBrowser(ctx context.Context, l OAuthLogin) (SignedIn, error) {
	if l.Out == nil {
		l.Out = Stdout
	}
	if l.Wait <= 0 {
		l.Wait = OAuthCallbackWait
	}
	if err := checkIssuer(l.Issuer); err != nil {
		return SignedIn{}, err
	}
	hc := oauthHTTPClient(l.Issuer)
	md, err := fetchOAuthMetadata(ctx, hc, l.Issuer)
	if err != nil {
		return SignedIn{}, err
	}
	key, err := EnsureDeviceKey(l.Addr)
	if err != nil {
		return SignedIn{}, err
	}

	var (
		redirect string
		answer   func(callbackCheck, string) (callbackResult, error)
	)
	if l.ReadCode != nil {
		redirect = oauthas.CodeRedirectURI(md.Issuer)
		answer = func(check callbackCheck, authURL string) (callbackResult, error) {
			return l.awaitCode(ctx, check, authURL)
		}
	} else {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return SignedIn{}, fmt.Errorf("browser sign-in: listen on loopback: %w", err)
		}
		defer func() { _ = lis.Close() }()
		redirect = fmt.Sprintf("http://127.0.0.1:%d/callback", lis.Addr().(*net.TCPAddr).Port)
		answer = func(check callbackCheck, authURL string) (callbackResult, error) {
			return l.awaitLoopback(ctx, lis, check, authURL)
		}
	}

	clientID, err := ensureOAuthClient(ctx, hc, md, l, redirect)
	if err != nil {
		return SignedIn{}, err
	}

	verifier, err := randomToken(32) // 43 characters (RFC 7636 §4.1)
	if err != nil {
		return SignedIn{}, err
	}
	state, err := randomToken(24)
	if err != nil {
		return SignedIn{}, err
	}
	authURL, err := authorizeURL(md.AuthorizationEndpoint, url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	})
	if err != nil {
		return SignedIn{}, err
	}

	res, err := answer(callbackCheck{state: state, issuer: md.Issuer, requireIss: md.IssParameter}, authURL)
	if err != nil {
		return SignedIn{}, err
	}
	if res.err != nil {
		return SignedIn{}, res.err
	}

	tok, err := exchangeOAuthCode(ctx, hc, md, key, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"code_verifier": {verifier},
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
	})
	if err != nil {
		var te *oauthTokenError
		if errors.As(err, &te) && te.body.Code == "invalid_client" && l.SaveClientID != nil {
			// The client went away between the authorization and the
			// exchange: forget it, so the next login registers anew.
			_ = l.SaveClientID("")
		}
		if deviceKeyRefused(err) && discardRevokedDeviceKey(l.Addr, key.Thumbprint()) {
			// The console refuses this key for the account for good (a
			// revoked device): the next login makes a new one. Only on the
			// auth plugin's own answer — an invalid_dpop_proof is the
			// gateway's check of the proof (a skewed clock, a replay), and the
			// key, with the session it may still hold, stays.
			return SignedIn{}, fmt.Errorf("%w\n  this machine's key was revoked on the console and has been removed; run `w17ctl login` again to sign in with a new key", err)
		}
		return SignedIn{}, err
	}
	signed := SignedIn{Token: tok.AccessToken}
	if strings.EqualFold(tok.TokenType, dpop.AuthScheme) {
		signed.Device = key.Thumbprint()
	}
	if tok.ExpiresIn > 0 {
		signed.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	}
	return signed, nil
}

// checkIssuer refuses an issuer w17ctl must not send a code or a proof to:
// https only, plain http only on a loopback host (a local console).
func checkIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || !u.IsAbs() || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("browser sign-in: issuer %q is not an absolute URL (set --issuer or %s)", issuer, EnvOAuthIssuerVar)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackTarget(u.Host)) {
		return fmt.Errorf("browser sign-in: issuer %q must be https (plain http only on a loopback host)", issuer)
	}
	return nil
}

func fetchOAuthMetadata(ctx context.Context, hc *http.Client, issuer string) (oauthMetadata, error) {
	var md oauthMetadata
	where := issuer + oauthas.MetadataPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, where, nil)
	if err != nil {
		return md, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return md, fmt.Errorf("%w: cannot reach %s: %v\n"+
			"  the issuer is derived from the console address; name it with --issuer or %s,\n"+
			"  or sign in with a password: w17ctl login --no-browser", ErrOAuthUnavailable, where, err, EnvOAuthIssuerVar)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return md, fmt.Errorf("%w: %s answered HTTP %d — it serves no OAuth authorization server (the auth plugin's oauth_server feature)\n"+
			"  sign in with a password instead: w17ctl login --no-browser", ErrOAuthUnavailable, where, resp.StatusCode)
	}
	if err := json.Unmarshal(body, &md); err != nil {
		return md, fmt.Errorf("%w: %s is not an OAuth metadata document: %w", ErrOAuthUnavailable, where, err)
	}
	// RFC 8414 §3.3: the document must name the issuer it was fetched for,
	// or another server's endpoints could be passed off as this one's.
	if md.Issuer != issuer {
		return md, fmt.Errorf("browser sign-in: %s names issuer %q, not %q — refusing it (RFC 8414 §3.3)", where, md.Issuer, issuer)
	}
	switch {
	case md.AuthorizationEndpoint == "" || md.TokenEndpoint == "":
		return md, fmt.Errorf("%w: %s names no authorization or token endpoint", ErrOAuthUnavailable, where)
	case !slices.Contains(md.CodeChallengeMethods, "S256"):
		return md, fmt.Errorf("%w: the authorization server does not support PKCE S256", ErrOAuthUnavailable)
	case !slices.Contains(md.DPoPAlgs, "ES256"):
		return md, fmt.Errorf("%w: the authorization server takes no DPoP proofs, so it cannot bind a session to this machine's key\n"+
			"  sign in with a password instead: w17ctl login --no-browser", ErrOAuthUnavailable)
	}
	for _, ep := range []string{md.AuthorizationEndpoint, md.TokenEndpoint} {
		if err := checkIssuer(ep); err != nil {
			return md, fmt.Errorf("browser sign-in: endpoint %q: must be https", ep)
		}
	}
	return md, nil
}

// oauthRegisteredRedirect is what w17ctl registers: a loopback redirect
// matches with any port at authorization time (RFC 8252 §7.3), so one
// registration serves every run's random port.
const oauthRegisteredRedirect = "http://127.0.0.1/callback"

// ensureOAuthClient returns a client id the server knows. A cached one is
// checked first — an authorization request with an unknown client shows the
// user an error page and nothing comes back to w17ctl, which would leave the
// login waiting the whole five minutes — and registered anew when the server
// does not know it (a console reset, a recycled local port, a disabled
// client).
func ensureOAuthClient(ctx context.Context, hc *http.Client, md oauthMetadata, l OAuthLogin, redirect string) (string, error) {
	if l.ClientID != "" {
		known, err := oauthClientKnown(ctx, hc, md.AuthorizationEndpoint, l.ClientID, redirect)
		if err != nil {
			return "", err
		}
		if known {
			return l.ClientID, nil
		}
	}
	id, err := registerOAuthClient(ctx, hc, md, redirect)
	if err != nil {
		return "", err
	}
	if l.SaveClientID != nil {
		if err := l.SaveClientID(id); err != nil {
			fmt.Fprintf(Stderr, "warning: could not keep the registered OAuth client: %v\n", err)
		}
	}
	return id, nil
}

// oauthClientKnown asks the authorization endpoint whether it knows clientID,
// without starting an authorization: the request carries an unsupported
// response_type. A server checks the client and its redirect first and, when
// they are fine, sends every other error back to the redirect (RFC 6749
// §4.1.2.1) — a 302 to our own redirect means the client is known; an error
// page means it is not.
func oauthClientKnown(ctx context.Context, hc *http.Client, endpoint, clientID, redirect string) (bool, error) {
	probe, err := authorizeURL(endpoint, url.Values{
		"response_type": {"w17ctl_client_check"},
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
	})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
	if err != nil {
		return false, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("browser sign-in: authorization endpoint: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		return false, nil
	}
	return strings.HasPrefix(resp.Header.Get("Location"), redirect), nil
}

// registerOAuthClient registers w17ctl with the loopback redirect and, for
// the sign-in with a code, the console's code page too: one client then
// serves both, and a console without the code page still takes the loopback
// one alone.
func registerOAuthClient(ctx context.Context, hc *http.Client, md oauthMetadata, redirect string) (string, error) {
	redirects := []string{oauthRegisteredRedirect}
	if redirect == oauthas.CodeRedirectURI(md.Issuer) {
		redirects = append(redirects, redirect)
	}
	if md.RegistrationEndpoint == "" {
		return "", fmt.Errorf("%w: the authorization server does not let clients register (dynamic registration is off)\n"+
			"  sign in with a password instead: w17ctl login --no-browser", ErrOAuthUnavailable)
	}
	if err := checkIssuer(md.RegistrationEndpoint); err != nil {
		return "", fmt.Errorf("browser sign-in: registration endpoint %q: must be https", md.RegistrationEndpoint)
	}
	body, err := json.Marshal(oauthas.ClientMetadata{
		RedirectURIs:            redirects,
		ClientName:              "w17ctl",
		SoftwareID:              "w17ctl",
		SoftwareVersion:         Version,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code"},
		ResponseTypes:           []string{"code"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("browser sign-in: register the client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("browser sign-in: the console refused to register w17ctl as a client: %s", describeOAuthError(resp.StatusCode, raw))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ClientID == "" {
		return "", fmt.Errorf("browser sign-in: the registration answer names no client_id")
	}
	return out.ClientID, nil
}

func describeOAuthError(status int, raw []byte) string {
	var oe oauthErrorBody
	if json.Unmarshal(raw, &oe) == nil && oe.Code != "" {
		return oe.String()
	}
	return fmt.Sprintf("HTTP %d", status)
}

// authorizeURL adds params to endpoint, keeping any query it already has.
func authorizeURL(endpoint string, params url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("browser sign-in: authorization endpoint %q: %w", endpoint, err)
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// callbackResult is what the browser brought back.
type callbackResult struct {
	code string
	err  error
}

// callbackHandler serves the loopback redirect. The first request to
// /callback decides the login, whatever it carries: a wrong state is refused
// rather than waited past, because nothing but the browser this login opened
// should know the port, and a request that does is worth stopping for.
// awaitLoopback opens authURL in this machine's browser and waits for the
// answer on the loopback redirect.
func (l OAuthLogin) awaitLoopback(ctx context.Context, lis net.Listener, check callbackCheck, authURL string) (callbackResult, error) {
	results := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(check, callbackSink(results)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(lis) }()
	defer func() { _ = srv.Close() }()

	fmt.Fprintf(l.Out, "Open this URL to sign in:\n\n  %s\n\n", authURL)
	if err := OpenBrowserFn(authURL); err == nil {
		fmt.Fprintln(l.Out, "(opened in your browser; waiting for you to approve the sign-in)")
	} else {
		fmt.Fprintln(l.Out, "(could not open a browser — open the URL yourself; waiting for you to approve the sign-in)")
	}

	wait := time.NewTimer(l.Wait)
	defer wait.Stop()
	select {
	case res := <-results:
		return res, nil
	case <-wait.C:
		return callbackResult{}, fmt.Errorf("browser sign-in: nothing came back from the browser within %s\n"+
			"  run `w17ctl login` again; if w17ctl runs on another machine than your browser, choose the sign-in with a code (`w17ctl login --method code`)", l.Wait)
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	}
}

// awaitCode shows authURL to open in any browser and reads the code the
// console's code page shows after the approval: `code#state`, whose state is
// checked as a redirect's would be. The page has already checked the issuer.
func (l OAuthLogin) awaitCode(ctx context.Context, check callbackCheck, authURL string) (callbackResult, error) {
	fmt.Fprintf(l.Out, "Open this URL in a browser on any device, sign in and approve:\n\n  %s\n\n"+
		"The console then shows a code. Paste it here.\n", authURL)
	check.requireIss = false
	type read struct {
		res callbackResult
		err error
	}
	got := make(chan read, 1)
	go func() {
		for range 3 {
			raw, err := l.ReadCode()
			if err != nil {
				got <- read{err: err}
				return
			}
			code, state, ok := strings.Cut(strings.TrimSpace(raw), "#")
			if !ok || code == "" || state == "" {
				fmt.Fprintln(l.Out, "That is not the code the console shows — copy the whole line, with the # in it.")
				continue
			}
			got <- read{res: check.result(url.Values{"code": {code}, "state": {state}})}
			return
		}
		got <- read{err: errors.New("browser sign-in: no code was entered — run `w17ctl login` again")}
	}()

	wait := time.NewTimer(l.Wait)
	defer wait.Stop()
	select {
	case r := <-got:
		return r.res, r.err
	case <-wait.C:
		return callbackResult{}, fmt.Errorf("browser sign-in: no code within %s — run `w17ctl login` again", l.Wait)
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	}
}

// callbackCheck is what an authorization response must carry to be this
// login's: the state it sent and, when the server announces it, its issuer.
type callbackCheck struct {
	state, issuer string
	requireIss    bool
}

// result reads an authorization response's query, from the loopback or
// pasted, into the code or the refusal.
func (c callbackCheck) result(q url.Values) callbackResult {
	var res callbackResult
	switch {
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.state)) != 1:
		res.err = errors.New("browser sign-in: the browser came back with a state this login did not send — refused (start again with `w17ctl login`)")
	case c.requireIss && q.Get("iss") == "":
		res.err = errors.New("browser sign-in: the answer carries no `iss`, which this server sends with every one (RFC 9207) — refused")
	case q.Get("iss") != "" && q.Get("iss") != c.issuer:
		// RFC 9207: the answer names the server that issued it.
		res.err = fmt.Errorf("browser sign-in: the answer came from issuer %q, not %q — refused", q.Get("iss"), c.issuer)
	case q.Get("error") == "access_denied":
		res.err = fmt.Errorf("%w (start again with `w17ctl login` to retry)", ErrOAuthDenied)
	case q.Get("error") != "":
		res.err = fmt.Errorf("browser sign-in: the console refused the sign-in: %s", oauthErrorBody{Code: q.Get("error"), Description: q.Get("error_description")})
	case q.Get("code") == "":
		res.err = errors.New("browser sign-in: the browser came back without a code")
	default:
		res.code = q.Get("code")
	}
	return res
}

// callbackSink delivers the first answer, from whichever way it came (the
// loopback or a paste); it reports whether this one was that first.
func callbackSink(results chan<- callbackResult) func(callbackResult) bool {
	var once sync.Once
	return func(r callbackResult) (first bool) {
		once.Do(func() {
			results <- r
			first = true
		})
		return first
	}
}

func callbackHandler(check callbackCheck, finish func(callbackResult) bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		res := check.result(r.URL.Query())
		if !finish(res) {
			http.Error(w, "this sign-in is already finished", http.StatusGone)
			return
		}
		title, msg := "Signed in", "w17ctl has your sign-in. You can close this window."
		status := http.StatusOK
		if res.err != nil {
			title, msg, status = "Sign-in failed", res.err.Error(), http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>`+title+`</title>`+
			`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">`+
			`<h1 style="font-size:1.25rem">`+title+`</h1><p>`+html.EscapeString(msg)+`</p></body>`)
	})
	return mux
}

// oauthToken is a successful token response.
type oauthToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// oauthTokenError is a refusal from the token endpoint.
// oauthDeviceKeyRefused is the auth plugin's answer to a valid proof whose
// key cannot sign in for the account (an extension error, RFC 6749 §8.5).
const oauthDeviceKeyRefused = "w17_device_key_refused"

// deviceKeyRefused: the token endpoint refused this key for the account for
// good — the one answer on which the key is discarded.
func deviceKeyRefused(err error) bool {
	var te *oauthTokenError
	return errors.As(err, &te) && te.body.Code == oauthDeviceKeyRefused
}

type oauthTokenError struct {
	status int
	body   oauthErrorBody
}

func (e *oauthTokenError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.status)
	if e.body.Code != "" {
		msg = e.body.String()
	}
	hint := ""
	switch e.status {
	case http.StatusTooManyRequests:
		hint = "\n  too many sign-ins from this address; wait a minute and run `w17ctl login` again"
	case http.StatusServiceUnavailable:
		hint = "\n  the console could not answer; try again, or `w17ctl login --no-browser`"
	}
	return "browser sign-in: the console refused the code exchange: " + msg + hint
}

// exchangeOAuthCode redeems the code at the token endpoint with a DPoP proof
// of key — unbound (no ath), htm POST, htu the token endpoint — and follows
// one `use_dpop_nonce` (RFC 9449 §8: a 400 carrying DPoP-Nonce).
func exchangeOAuthCode(ctx context.Context, hc *http.Client, md oauthMetadata, key *dpop.Key, form url.Values) (oauthToken, error) {
	nonce := ""
	for attempt := 0; ; attempt++ {
		proof, err := dpop.NewProof(key, http.MethodPost, md.TokenEndpoint, "", nonce)
		if err != nil {
			return oauthToken{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return oauthToken{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set(dpop.HeaderProof, proof)
		resp, err := hc.Do(req)
		if err != nil {
			return oauthToken{}, fmt.Errorf("browser sign-in: token endpoint: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var tok oauthToken
			if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
				return oauthToken{}, errors.New("browser sign-in: the token answer carries no access_token")
			}
			return tok, nil
		}
		var oe oauthErrorBody
		_ = json.Unmarshal(raw, &oe)
		fresh := resp.Header.Get(dpop.HeaderNonce)
		if oe.Code == dpop.ChallengeUseNonce && fresh != "" && fresh != nonce && attempt == 0 {
			nonce = fresh
			continue
		}
		return oauthToken{}, &oauthTokenError{status: resp.StatusCode, body: oe}
	}
}
