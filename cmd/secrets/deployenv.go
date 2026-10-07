package secrets

// The deploy env files of an infrastructure environment (docs/decisions/
// infra-targets.md §4.3, §8): deploy/<env>/services/<service>/.env, filled by a
// person from the generated .env.example beside it, committed only as the
// sops-encrypted .env.enc. One age key per environment; its public half lives in
// deploy/.sops.yaml, its private half in a password manager and the GitHub
// environment secret SOPS_AGE_KEY — never on disk unless asked for.
//
// The format is sops's own (sdk/go/service/secret/sopsenv, proven against the
// sops CLI both ways), so `sops decrypt` works on every file this writes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"gopkg.in/yaml.v3"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"
)

// envFlags is what every deploy-env command takes.
type envFlags struct {
	Env      string `name:"env" help:"Environment (deploy/<env>/). Empty = every environment under deploy/."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file (locates the project root)."`
}

// keyFlag names an age key file in addition to SOPS_AGE_KEY / SOPS_AGE_KEY_FILE.
type keyFlag struct {
	Key string `name:"key" placeholder:"FILE" help:"Age key file. Default: $SOPS_AGE_KEY, then $SOPS_AGE_KEY_FILE."`
}

func projectRoot(lockPath string) (string, error) {
	abs, err := filepath.Abs(lockPath)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("no lock at %s — run this from the project root", lockPath)
	}
	return filepath.Dir(filepath.Dir(abs)), nil
}

// environments lists deploy/<env> directories that hold rendered services.
func environments(root, only string) ([]string, error) {
	if only != "" {
		if _, err := os.Stat(filepath.Join(root, "deploy", only, "services")); err != nil {
			return nil, fmt.Errorf("deploy/%s/services does not exist — `w17ctl infra show` lists the environments, `w17ctl codegen` renders them", only)
		}
		return []string{only}, nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "deploy"))
	if err != nil {
		return nil, fmt.Errorf("no deploy/ directory — declare an infrastructure (`w17ctl infra init`) and run `w17ctl codegen`")
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(root, "deploy", e.Name(), "services")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no environment under deploy/ — run `w17ctl codegen` after `w17ctl infra init`")
	}
	return out, nil
}

// envFile is one service's three files.
type envFile struct {
	Service string
	Example string // .env.example (generated)
	Plain   string // .env (the person's, gitignored)
	Sealed  string // .env.enc (committed)
}

func envFiles(root, env string) ([]envFile, error) {
	matches, err := filepath.Glob(filepath.Join(root, "deploy", env, "services", "*", ".env.example"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var out []envFile
	for _, ex := range matches {
		dir := filepath.Dir(ex)
		out = append(out, envFile{
			Service: filepath.Base(dir), Example: ex,
			Plain: filepath.Join(dir, ".env"), Sealed: filepath.Join(dir, ".env.enc"),
		})
	}
	return out, nil
}

// ── .sops.yaml ──────────────────────────────────────────────────────────────────────

const deploySopsConfigHeader = "# Managed by `w17ctl secrets keygen`: one age public key per environment. The private\n" +
	"# keys are NOT here — they live in a password manager and the GitHub environment secret\n" +
	"# SOPS_AGE_KEY. The rules also let the sops CLI work on these files from deploy/.\n"

type sopsRule struct {
	PathRegex string `yaml:"path_regex"`
	Age       string `yaml:"age"`
}

type deploySopsConfig struct {
	CreationRules []sopsRule `yaml:"creation_rules"`
}

func deploySopsConfigPath(root string) string { return filepath.Join(root, "deploy", ".sops.yaml") }

func envRegex(env string) string { return env + `/.*\.env(\.enc)?$` }

func readSopsConfig(root string) (*deploySopsConfig, error) {
	b, err := os.ReadFile(deploySopsConfigPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return &deploySopsConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c deploySopsConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("deploy/.sops.yaml: %w", err)
	}
	return &c, nil
}

func (c *deploySopsConfig) recipients(env string) []string {
	for _, r := range c.CreationRules {
		if r.PathRegex == envRegex(env) {
			var out []string
			for _, a := range strings.Split(r.Age, ",") {
				if a = strings.TrimSpace(a); a != "" {
					out = append(out, a)
				}
			}
			return out
		}
	}
	return nil
}

func (c *deploySopsConfig) set(env string, recipients []string) {
	rule := sopsRule{PathRegex: envRegex(env), Age: strings.Join(recipients, ",")}
	for i, r := range c.CreationRules {
		if r.PathRegex == rule.PathRegex {
			c.CreationRules[i] = rule
			return
		}
	}
	c.CreationRules = append(c.CreationRules, rule)
	sort.Slice(c.CreationRules, func(i, j int) bool { return c.CreationRules[i].PathRegex < c.CreationRules[j].PathRegex })
}

func writeSopsConfig(root string, c *deploySopsConfig) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(deploySopsConfigPath(root), append([]byte(deploySopsConfigHeader), b...), 0o644)
}

func requireRecipients(root, env string) ([]string, error) {
	c, err := readSopsConfig(root)
	if err != nil {
		return nil, err
	}
	r := c.recipients(env)
	if len(r) == 0 {
		return nil, fmt.Errorf("environment %s has no key in deploy/.sops.yaml — run `w17ctl secrets keygen --env %s` once", env, env)
	}
	return r, nil
}

// ── keygen ──────────────────────────────────────────────────────────────────────────

// KeygenCmd is `w17ctl secrets keygen --env <env>`.
type KeygenCmd struct {
	Env      string `name:"env" required:"" help:"Environment the key is for."`
	Save     string `name:"save" placeholder:"PATH" help:"Also write the private key to this file (refused inside the repository unless gitignored)."`
	Force    bool   `name:"force" help:"Replace the environment's existing key. Every .env.enc must then be re-sealed: run 'secrets rekey' with the OLD key first."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file (locates the project root)."`
}

func (c *KeygenCmd) Run() error {
	root, err := projectRoot(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets keygen: %w", err)
	}
	if _, err := environments(root, c.Env); err != nil {
		return fmt.Errorf("secrets keygen: %w", err)
	}
	cfg, err := readSopsConfig(root)
	if err != nil {
		return fmt.Errorf("secrets keygen: %w", err)
	}
	if existing := cfg.recipients(c.Env); len(existing) > 0 && !c.Force {
		return fmt.Errorf("secrets keygen: environment %s already has a key (%s). A new key makes every sealed file unreadable to it; "+
			"to rotate, add the new public key with `w17ctl secrets rekey --env %s --add <age1…>` while you still hold the old one", c.Env, existing[0], c.Env)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("secrets keygen: %w", err)
	}
	if c.Save != "" {
		if err := refuseTrackedPath(root, c.Save); err != nil {
			return fmt.Errorf("secrets keygen: %w", err)
		}
		if err := os.WriteFile(c.Save, []byte(id.String()+"\n"), 0o600); err != nil {
			return fmt.Errorf("secrets keygen: %w", err)
		}
	}
	cfg.set(c.Env, []string{id.Recipient().String()})
	if err := writeSopsConfig(root, cfg); err != nil {
		return fmt.Errorf("secrets keygen: %w", err)
	}
	w := core.Stdout
	fmt.Fprintf(w, "age key for environment %s\n\n", c.Env)
	fmt.Fprintf(w, "  PRIVATE (shown once — nothing kept it%s):\n    %s\n\n", map[bool]string{true: " except " + c.Save, false: ""}[c.Save != ""], id.String())
	fmt.Fprintf(w, "  public (written to deploy/.sops.yaml — commit it):\n    %s\n\n", id.Recipient().String())
	fmt.Fprintf(w, "Store the private key in your password manager AND as the GitHub environment secret\n"+
		"SOPS_AGE_KEY of environment %s. Lose it and the sealed files cannot be opened again.\n", c.Env)
	fmt.Fprintf(w, "Next: fill deploy/%s/services/*/.env from the .env.example beside each, then\n"+
		"  SOPS_AGE_KEY=… w17ctl secrets seal --env %s\n", c.Env, c.Env)
	return nil
}

// refuseTrackedPath refuses a key file path inside the repository that git
// would not ignore — the private key must never be one `git add .` away.
func refuseTrackedPath(root, p string) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // outside the project
	}
	if !inGitWorkTree(root) {
		return nil
	}
	if !gitIgnored(root, rel) {
		return fmt.Errorf("%s is inside the repository and not gitignored — a private key there is one `git add` from being committed; save it elsewhere or ignore it first", rel)
	}
	return nil
}

func inGitWorkTree(root string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitIgnored: exit 0 = ignored; anything else reads as NOT ignored — the
// direction that refuses rather than the one that leaks.
func gitIgnored(root, rel string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", "--", rel)
	cmd.Dir = root
	return cmd.Run() == nil
}

// ── validation (§8.4) ───────────────────────────────────────────────────────────────

var (
	placeholders = []string{"CHANGE_ME", "REPLACE_ME", "s3://BUCKET/"}
	pgDSN        = regexp.MustCompile(`postgres(?:ql)?://([^:/@"\s]+):([^@"\s]*)@`)
)

// validate checks one environment's plaintext files before anything is sealed.
// Every problem is collected, so one run names them all.
func validate(root string, files []envFile) []string {
	var problems []string
	values := map[string]map[string]string{}
	for _, f := range files {
		rel, _ := filepath.Rel(root, f.Plain)
		plain, err := os.ReadFile(f.Plain)
		if errors.Is(err, os.ErrNotExist) {
			continue // nothing to seal for this service (seal decides whether that is fine)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		lines, err := sopsenv.Parse(plain)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		exLines, err := readLines(f.Example)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f.Example, err))
			continue
		}
		want := map[string]bool{}
		for _, l := range exLines {
			if !l.Comment {
				want[l.Key] = true
			}
		}
		seen := map[string]bool{}
		for _, l := range lines {
			if l.Comment {
				continue
			}
			if seen[l.Key] {
				problems = append(problems, fmt.Sprintf("%s: %s appears twice", rel, l.Key))
			}
			seen[l.Key] = true
			if !want[l.Key] {
				problems = append(problems, fmt.Sprintf("%s: %s is not in .env.example — a typo, or a key the service does not read", rel, l.Key))
			}
			if p := valueProblem(l.Value); p != "" {
				problems = append(problems, fmt.Sprintf("%s: %s %s", rel, l.Key, p))
			}
		}
		var missing []string
		for k := range want {
			if !seen[k] {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		for _, k := range missing {
			problems = append(problems, fmt.Sprintf("%s: %s is missing (it is in .env.example; an empty value is allowed, an absent key is not)", rel, k))
		}
		values[f.Service] = sopsenv.Values(lines)
	}
	problems = append(problems, crossFileProblems(values)...)
	return problems
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
// the one inside the DSN of every binary connecting as that role.
func crossFileProblems(values map[string]map[string]string) []string {
	pg := values["postgres"]
	if pg == nil {
		return nil
	}
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
				role, pw := m[1], m[2]
				pwKey := strings.ToUpper(strings.ReplaceAll(role, "-", "_")) + "_DB_PASSWORD"
				want, ok := pg[pwKey]
				if !ok {
					continue
				}
				if pw != want {
					problems = append(problems, fmt.Sprintf("deploy/…/services/%s/.env: %s connects as %s with a password that is not postgres/.env's %s", svc, k, role, pwKey))
				}
			}
		}
	}
	return problems
}

func readLines(path string) ([]sopsenv.Line, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return sopsenv.Parse(b)
}

func sameValues(a, b []sopsenv.Line) bool {
	va, vb := sopsenv.Values(a), sopsenv.Values(b)
	if len(va) != len(vb) {
		return false
	}
	for k, v := range va {
		if w, ok := vb[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ── seal ────────────────────────────────────────────────────────────────────────────

// SealCmd is `w17ctl secrets seal [--env]`.
type SealCmd struct {
	envFlags
	keyFlag
}

func (c *SealCmd) Run() error {
	root, err := projectRoot(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets seal: %w", err)
	}
	envs, err := environments(root, c.Env)
	if err != nil {
		return fmt.Errorf("secrets seal: %w", err)
	}
	ids, err := sopsenv.LoadIdentities(os.Getenv, nonEmpty(c.Key)...)
	if err != nil {
		return fmt.Errorf("secrets seal: %w", err)
	}
	type job struct {
		env  string
		f    envFile
		rcpt []string
	}
	var jobs []job
	var problems []string
	for _, env := range envs {
		rcpt, err := requireRecipients(root, env)
		if err != nil {
			return fmt.Errorf("secrets seal: %w", err)
		}
		files, err := envFiles(root, env)
		if err != nil {
			return err
		}
		problems = append(problems, validate(root, files)...)
		for _, f := range files {
			rel, _ := filepath.Rel(root, f.Plain)
			if _, err := os.Stat(f.Plain); err != nil {
				if _, encErr := os.Stat(f.Sealed); encErr != nil {
					problems = append(problems, fmt.Sprintf("%s: missing — copy it from .env.example beside it and fill it in", rel))
				}
				continue
			}
			if inGitWorkTree(root) && !gitIgnored(root, rel) {
				problems = append(problems, fmt.Sprintf("%s: not gitignored — the plaintext would be committed; add `.env` to .gitignore", rel))
			}
			jobs = append(jobs, job{env, f, rcpt})
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("secrets seal: nothing sealed —\n  %s", strings.Join(problems, "\n  "))
	}
	if len(ids) == 0 {
		fmt.Fprintln(core.Stdout, "no private key (SOPS_AGE_KEY / SOPS_AGE_KEY_FILE / --key): sealed files cannot be compared, so a .env newer than its .env.enc is re-sealed and the rest left as they are")
	}
	sealed, kept := 0, 0
	for _, j := range jobs {
		rel, _ := filepath.Rel(root, j.f.Sealed)
		lines, err := readLines(j.f.Plain)
		if err != nil {
			return err
		}
		if unchanged, why := sealedIsCurrent(j.f, lines, ids); unchanged {
			kept++
			fmt.Fprintf(core.Stdout, "  unchanged  %s (%s)\n", rel, why)
			continue
		}
		enc, err := sopsenv.Encrypt(lines, j.rcpt, time.Now())
		if err != nil {
			return fmt.Errorf("secrets seal: %s: %w", rel, err)
		}
		if err := os.WriteFile(j.f.Sealed, enc, 0o644); err != nil {
			return fmt.Errorf("secrets seal: %w", err)
		}
		sealed++
		fmt.Fprintf(core.Stdout, "  sealed     %s\n", rel)
	}
	fmt.Fprintf(core.Stdout, "secrets seal: %d sealed, %d unchanged — commit the .env.enc files\n", sealed, kept)
	return nil
}

// sealedIsCurrent: the existing .env.enc already holds exactly these values.
// sops output is never the same twice (fresh data key and IVs), so re-sealing
// an unchanged file would rewrite every .env.enc on every run.
func sealedIsCurrent(f envFile, lines []sopsenv.Line, ids []age.Identity) (bool, string) {
	enc, err := os.ReadFile(f.Sealed)
	if err != nil {
		return false, ""
	}
	if len(ids) > 0 {
		cur, err := sopsenv.Decrypt(enc, ids)
		if err != nil {
			return false, ""
		}
		return sameValues(cur, lines), "same values"
	}
	ps, err1 := os.Stat(f.Plain)
	es, err2 := os.Stat(f.Sealed)
	if err1 != nil || err2 != nil {
		return false, ""
	}
	return !ps.ModTime().After(es.ModTime()), "older than the sealed copy"
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func requireIdentities(verb, key string) ([]age.Identity, error) {
	ids, err := sopsenv.LoadIdentities(os.Getenv, nonEmpty(key)...)
	if err != nil {
		return nil, fmt.Errorf("secrets %s: %w", verb, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("secrets %s: no private key — set SOPS_AGE_KEY (or SOPS_AGE_KEY_FILE), or pass --key", verb)
	}
	return ids, nil
}

// ── unseal ──────────────────────────────────────────────────────────────────────────

// UnsealCmd is `w17ctl secrets unseal [--env]`.
type UnsealCmd struct {
	envFlags
	keyFlag
	Force bool   `name:"force" help:"Overwrite a .env whose values differ from the sealed copy."`
	Print string `name:"print" placeholder:"SERVICE" help:"Write ONE service's decrypted file to stdout instead (needs --env) — what the deploy workflow pipes into ssh, so the plaintext never touches the runner's disk."`
	JSON  bool   `name:"json" help:"With --print: one JSON object of KEY: value (comments dropped) — what a cloud deploy writes into its secret store."`
}

func (c *UnsealCmd) Run() error {
	root, err := projectRoot(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets unseal: %w", err)
	}
	envs, err := environments(root, c.Env)
	if err != nil {
		return fmt.Errorf("secrets unseal: %w", err)
	}
	ids, err := requireIdentities("unseal", c.Key)
	if err != nil {
		return err
	}
	if c.JSON && c.Print == "" {
		return errors.New("secrets unseal: --json needs --print SERVICE")
	}
	if c.Print != "" {
		if c.Env == "" {
			return errors.New("secrets unseal: --print needs --env")
		}
		f := filepath.Join(root, "deploy", c.Env, "services", c.Print, ".env.enc")
		enc, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("secrets unseal: %w", err)
		}
		lines, err := sopsenv.Decrypt(enc, ids)
		if err != nil {
			return fmt.Errorf("secrets unseal: deploy/%s/services/%s/.env.enc: %w", c.Env, c.Print, err)
		}
		if c.JSON {
			return json.NewEncoder(core.Stdout).Encode(sopsenv.Values(lines))
		}
		_, err = core.Stdout.Write(sopsenv.Format(lines))
		return err
	}
	for _, env := range envs {
		files, err := envFiles(root, env)
		if err != nil {
			return err
		}
		for _, f := range files {
			rel, _ := filepath.Rel(root, f.Plain)
			enc, err := os.ReadFile(f.Sealed)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			lines, err := sopsenv.Decrypt(enc, ids)
			if err != nil {
				return fmt.Errorf("secrets unseal: %s: %w", rel, err)
			}
			if cur, err := readLines(f.Plain); err == nil && !c.Force && !sameValues(cur, lines) {
				return fmt.Errorf("secrets unseal: %s differs from its sealed copy — it may hold edits not sealed yet; seal them, or pass --force to overwrite", rel)
			}
			if err := os.WriteFile(f.Plain, sopsenv.Format(lines), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(core.Stdout, "  unsealed  %s\n", rel)
		}
	}
	return nil
}

// ── check ───────────────────────────────────────────────────────────────────────────

// CheckCmd is `w17ctl secrets check [--env]` — no private key needed; CI runs it.
type CheckCmd struct {
	envFlags
}

func (c *CheckCmd) Run() error {
	root, err := projectRoot(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets check: %w", err)
	}
	envs, err := environments(root, c.Env)
	if err != nil {
		return fmt.Errorf("secrets check: %w", err)
	}
	var problems []string
	checked := 0
	for _, env := range envs {
		rcpt, err := requireRecipients(root, env)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		files, err := envFiles(root, env)
		if err != nil {
			return err
		}
		for _, f := range files {
			rel, _ := filepath.Rel(root, f.Sealed)
			enc, err := os.ReadFile(f.Sealed)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: missing — fill .env and run `w17ctl secrets seal --env %s`", rel, env))
				continue
			}
			s, err := sopsenv.Read(enc)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
				continue
			}
			ex, err := readLines(f.Example)
			if err != nil {
				return err
			}
			have := map[string]bool{}
			for _, k := range s.Keys() {
				have[k] = true
			}
			for _, l := range ex {
				if !l.Comment && !have[l.Key] {
					problems = append(problems, fmt.Sprintf("%s: %s is in .env.example but not sealed — unseal, add it, seal", rel, l.Key))
				}
				delete(have, l.Key)
			}
			for k := range have {
				problems = append(problems, fmt.Sprintf("%s: %s is sealed but no longer in .env.example", rel, k))
			}
			if got := s.Recipients(); strings.Join(got, ",") != strings.Join(rcpt, ",") {
				problems = append(problems, fmt.Sprintf("%s: sealed for %s, deploy/.sops.yaml names %s — run `w17ctl secrets rekey --env %s`", rel, strings.Join(got, ","), strings.Join(rcpt, ","), env))
			}
			checked++
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("secrets check: %d problem(s) —\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	fmt.Fprintf(core.Stdout, "secrets check: %d sealed file(s) match their .env.example and key\n", checked)
	return nil
}

// ── rekey ───────────────────────────────────────────────────────────────────────────

// RekeyCmd is `w17ctl secrets rekey [--env]`: re-seal every file's data key for
// the recipients deploy/.sops.yaml names (after `--add` / `--remove`).
type RekeyCmd struct {
	envFlags
	keyFlag
	Add    []string `name:"add" placeholder:"AGE1…" help:"Add this public key to the environment first (key rotation: add the new, rekey, then --remove the old)."`
	Remove []string `name:"remove" placeholder:"AGE1…" help:"Remove this public key from the environment first."`
}

func (c *RekeyCmd) Run() error {
	root, err := projectRoot(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets rekey: %w", err)
	}
	envs, err := environments(root, c.Env)
	if err != nil {
		return fmt.Errorf("secrets rekey: %w", err)
	}
	if (len(c.Add) > 0 || len(c.Remove) > 0) && len(envs) != 1 {
		return errors.New("secrets rekey: --add / --remove change one environment's key — name it with --env")
	}
	ids, err := requireIdentities("rekey", c.Key)
	if err != nil {
		return err
	}
	cfg, err := readSopsConfig(root)
	if err != nil {
		return err
	}
	for _, env := range envs {
		rcpt := cfg.recipients(env)
		for _, a := range c.Add {
			if _, err := age.ParseX25519Recipient(a); err != nil {
				return fmt.Errorf("secrets rekey: --add %s: %w", a, err)
			}
			if !contains(rcpt, a) {
				rcpt = append(rcpt, a)
			}
		}
		var kept []string
		for _, r := range rcpt {
			if !contains(c.Remove, r) {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("secrets rekey: removing every key of %s would make its files unreadable", env)
		}
		files, err := envFiles(root, env)
		if err != nil {
			return err
		}
		// Re-seal first, write .sops.yaml last: a failure part-way leaves the
		// config naming the keys the files still open with.
		changed := 0
		for _, f := range files {
			rel, _ := filepath.Rel(root, f.Sealed)
			enc, err := os.ReadFile(f.Sealed)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			s, err := sopsenv.Read(enc)
			if err != nil {
				return fmt.Errorf("secrets rekey: %s: %w", rel, err)
			}
			if strings.Join(s.Recipients(), ",") == strings.Join(kept, ",") {
				continue
			}
			out, err := sopsenv.Rekey(enc, ids, kept)
			if err != nil {
				return fmt.Errorf("secrets rekey: %s: %w", rel, err)
			}
			if err := os.WriteFile(f.Sealed, out, 0o644); err != nil {
				return err
			}
			changed++
			fmt.Fprintf(core.Stdout, "  rekeyed  %s\n", rel)
		}
		cfg.set(env, kept)
		fmt.Fprintf(core.Stdout, "secrets rekey: %s — %d file(s) re-sealed for %d key(s)\n", env, changed, len(kept))
	}
	return writeSopsConfig(root, cfg)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
