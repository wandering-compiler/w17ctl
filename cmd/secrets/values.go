package secrets

// The checks an environment's values are held to before a deploy takes them:
// what the server's bash (it sources the env files) or the service would do
// with each value, and the database role passwords that must agree across
// services. `w17ctl secrets fetch` (and so every deploy) runs them over the
// values the console hands out; nothing is written while one fails.

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/fieldcrypt"
)

var (
	placeholders = []string{"CHANGE_ME", "REPLACE_ME", "s3://BUCKET/"}
	pgDSN        = regexp.MustCompile(`postgres(?:ql)?://([^:/@"\s]+):([^@"\s]*)@([^:/"\s]+)`)
	// The user part may hold a `user:password` pair, which the token check
	// names rather than reading `user` as the host.
	natsDSN = regexp.MustCompile(`nats://(?:([^/@"\s]*)@)?([^:/@,"\s]+)`)
	// urlSecret: a secret that travels inside a URL (a token as a DSN's user
	// part, a password after it): at least 32 characters, and only those a URL
	// carries unescaped (RFC 3986 unreserved), so the DSN and the server hold
	// the same string with nothing to percent-encode.
	urlSecret = regexp.MustCompile(`^[A-Za-z0-9._~-]{32,}$`)
)

// natsTokenVar is the stack's nats token (services/nats/.env.example), the
// user part of every nats:// DSN that connects to it.
const natsTokenVar = "NATS_TOKEN"

// natsTokenProblem: a token nats would take but a DSN could not carry, or one
// too short to be a secret.
func natsTokenProblem(v string) string {
	if strings.Contains(v, "CHANGE_ME") || urlSecret.MatchString(v) {
		return ""
	}
	return "is not at least 32 letters, digits or -._~ — it travels as a URL's user part (`openssl rand -hex 32`)"
}

// nonceKeyProblem: a DPoP nonce key the binary refuses at boot — which a
// blue/green deploy finds only after the images moved.
func nonceKeyProblem(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.Contains(v, "CHANGE_ME") {
		return ""
	}
	// The encodings the binary takes (gateway dpopedge dpopNonceKey):
	// standard or URL base64, padded or not.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil {
			if len(b) < dpop.MinNonceKeyBytes {
				break
			}
			return ""
		}
	}
	return fmt.Sprintf("is not base64 of at least %d random bytes (`openssl rand -base64 32`)", dpop.MinNonceKeyBytes)
}

// redisDSNProblem: a redis:// DSN whose password is not a urlSecret — one
// with a character the URL would have to escape (the redis client decodes
// it, the server's own setting holds it raw, and the two then differ), or
// one too short to be a secret.
func redisDSNProblem(v string) string {
	if strings.Contains(v, "CHANGE_ME") || !strings.HasPrefix(v, "redis://") {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		return "is not a redis:// URL"
	}
	if pw, ok := u.User.Password(); ok && !urlSecret.MatchString(pw) {
		return "carries a password that is not at least 32 letters, digits or -._~ — it travels inside the URL (`openssl rand -hex 32`)"
	}
	return ""
}

// fieldKeysProblem: a keyring the service would refuse at boot — or boot
// with the public development key.
func fieldKeysProblem(v string) string {
	v = strings.Trim(v, `"`)
	if strings.Contains(v, "CHANGE_ME") {
		return "" // reported as a placeholder already
	}
	if strings.Contains(v, fieldcrypt.DevKeys[2:]) {
		return "is the public development key — give this environment its own: `w17ctl secrets field-key`"
	}
	// Not the parser's error: it quotes the entry, key material included, and
	// this message reaches a deploy's log.
	if _, err := fieldcrypt.Parse(v); err != nil {
		return "is not a keyring (<version>:<base64 key>, comma-separated) — `w17ctl secrets field-key` prints one"
	}
	return ""
}

// envVar names the environment a service reports (telemetry, Sentry). Its
// value `dev` is also what the binaries read as the development stack.
const envVar = "W17_ENV"

// envProblem: W17_ENV=dev in a deployment switches on the dev-only paths —
// the public development field-encryption keyring is accepted, and the DPoP
// edge runs single-replica with a per-boot nonce key.
func envProblem(v string) string {
	if strings.Trim(v, `"'`) != "dev" {
		return ""
	}
	return "is dev — the value that switches on the development stack's paths (the public development field-encryption keyring, single-replica DPoP replay protection); set it to this environment's name"
}

// deviceKeyVar is the sealed device key the deploy's `migrate --fetch` signs
// with (the infra generator's swarm.DeviceKeyVar): the key file, base64 on
// one line. Empty = none.
const deviceKeyVar = "W17_DEVICE_KEY"

// deviceKeyProblem: a device key the deploy would fail on — after the images
// moved — rather than here.
func deviceKeyProblem(v string) string {
	v = strings.Trim(v, `"`)
	if v == "" || strings.Contains(v, "CHANGE_ME") {
		return ""
	}
	if strings.Contains(v, "-----BEGIN") {
		return "is the PEM itself — a .env holds one line per value: put `base64 < <key file> | tr -d '\\n'` here"
	}
	pem, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "is not base64 (`base64 < <key file> | tr -d '\\n'` of the file `W17_DPOP_KEY=<file> w17ctl key public` made)"
	}
	if _, err := dpop.ParseKeyPEM(pem); err != nil {
		return "does not decode to a device key — want `base64 < <key file> | tr -d '\\n'` of the file `W17_DPOP_KEY=<file> w17ctl key public` made"
	}
	return ""
}

// valueProblem: what the server's bash (it sources these files) or a
// forgotten template would do with this value.
func valueProblem(v string) string {
	for _, p := range placeholders {
		if strings.Contains(v, p) {
			return "still holds the placeholder " + p
		}
	}
	if strings.HasPrefix(v, "#") {
		return "starts with # (a commented-out example, not a value)"
	}
	if strings.ContainsAny(v, "$`") {
		return "contains $ or a backtick — the server sources these files with bash, which would expand it"
	}
	quoted := len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"'
	inner := v
	if quoted {
		inner = v[1 : len(v)-1]
	}
	if strings.Contains(inner, `"`) {
		return `contains a double quote — the server's bash would end the value there`
	}
	if !quoted && strings.ContainsAny(v, " \t&;|<>()'") {
		return `has a space or one of & ; | < > ( ) ' outside double quotes — wrap the value in "…"`
	}
	return ""
}

// crossFileProblems: each Postgres role's password in postgres/.env must be
// the one inside the DSN of every binary connecting as that role, and the
// stack's nats token the user part of every nats:// DSN naming it.
func crossFileProblems(values map[string]map[string]string) []string {
	var problems []string
	services := make([]string, 0, len(values))
	for s := range values {
		services = append(services, s)
	}
	sort.Strings(services)
	for _, svc := range services {
		keys := make([]string, 0, len(values[svc]))
		for k := range values[svc] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, m := range pgDSN.FindAllStringSubmatch(values[svc][k], -1) {
				// The DSN's host is the database server it connects to — postgres, or
				// postgres-<name> with more than one — and that server's .env owns the
				// role's password.
				role, pw, host := m[1], m[2], m[3]
				pg := values[host]
				if pg == nil {
					continue
				}
				pwKey := strings.ToUpper(strings.ReplaceAll(role, "-", "_")) + "_DB_PASSWORD"
				want, ok := pg[pwKey]
				if !ok {
					continue
				}
				if pw != want {
					problems = append(problems, fmt.Sprintf("%s: %s connects as %s with a password that is not %s's %s", svc, k, role, host, pwKey))
				}
			}
			for _, m := range natsDSN.FindAllStringSubmatch(values[svc][k], -1) {
				token, host := m[1], m[2]
				want, ok := values[host][natsTokenVar]
				if !ok {
					continue
				}
				switch {
				case strings.Contains(token, ":"):
					problems = append(problems, fmt.Sprintf("%s: %s connects to %s with a user and password — %s takes its %s as the URL's user part alone (nats://<token>@%s:4222)", svc, k, host, host, natsTokenVar, host))
				case token == "":
					problems = append(problems, fmt.Sprintf("%s: %s connects to %s without a token — %s's %s is its user part (nats://<token>@%s:4222)", svc, k, host, host, natsTokenVar, host))
				case token != want:
					problems = append(problems, fmt.Sprintf("%s: %s connects to %s with a token that is not %s's %s", svc, k, host, host, natsTokenVar))
				}
			}
		}
	}
	return problems
}

// valuesProblems runs every check over an environment's values, service →
// key → value, as the console holds them (unquoted). It names the service and
// the key, never the value.
func valuesProblems(values map[string]map[string]string) []string {
	var problems []string
	services := make([]string, 0, len(values))
	for s := range values {
		services = append(services, s)
	}
	sort.Strings(services)
	for _, svc := range services {
		keys := make([]string, 0, len(values[svc]))
		for k := range values[svc] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := values[svc][k]
			var p string
			switch k {
			case fieldcrypt.EnvKeys:
				p = fieldKeysProblem(v)
			case envVar:
				p = envProblem(v)
			case deviceKeyVar:
				p = deviceKeyProblem(v)
			case natsTokenVar:
				p = natsTokenProblem(v)
			}
			if p == "" && strings.HasSuffix(k, "_DPOP_NONCE_KEY") {
				p = nonceKeyProblem(v)
			}
			if p == "" {
				p = redisDSNProblem(v)
			}
			if p == "" {
				p = rawValueProblem(v)
			}
			if p != "" {
				problems = append(problems, fmt.Sprintf("%s: %s %s", svc, k, p))
			}
		}
	}
	return append(problems, crossFileProblems(values)...)
}

// rawValueProblem is valueProblem for a value as the console holds it: not
// yet quoted for bash (EnvFile does that). The console refuses $, backticks,
// double quotes, backslashes and line breaks when a value is set; they are
// refused here again, because deploy-server.sh sources the file as root and
// a console that let one through would hand it a command.
func rawValueProblem(v string) string {
	if strings.ContainsAny(v, "$`\"\\\r\n\x00") {
		return "contains $, a backtick, a double quote, a backslash, a line break or NUL — the server's bash would expand or split it"
	}
	for _, p := range placeholders {
		if strings.Contains(v, p) {
			return "still holds the placeholder " + p
		}
	}
	if strings.HasPrefix(v, "#") {
		return "starts with # (a commented-out example, not a value)"
	}
	return ""
}
