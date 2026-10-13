package infra

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// PlanCmd is `w17ctl infra plan` — what a deploy of one environment would
// change, before it changes anything. deploy/ is generated and not in git, so
// this replaces the review of its diff:
//   - swarm: the rendered stack against what the last deploy left on the
//     server — files added, changed (with their diff) and removed, and the
//     image tag the colours run — plus a fingerprint of the server's state for
//     `w17ctl deploy --expect`;
//   - aws-ecs / gcp-cloudrun: `terraform plan` of this commit against the
//     environment's state, in the pinned Terraform image, with your own cloud
//     credentials.
type PlanCmd struct {
	Env         string `name:"env" required:"" help:"The environment to plan."`
	NoCodegen   bool   `name:"no-codegen" help:"Plan what deploy/<env>/ holds now instead of rendering it first."`
	StateBucket string `name:"state-bucket" env:"TF_STATE_BUCKET" help:"aws-ecs / gcp-cloudrun: the Terraform state bucket (deploy/<env>/terraform/bootstrap created it)."`
	LockPath    string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default."`
}

// planRun is the seam the tests replace: every local program (this binary,
// rsync, docker, git) goes through it.
var planRun = func(stdin io.Reader, stdout io.Writer, dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, stdin, stdout, os.Stderr
	return cmd.Run()
}

var planSelf = os.Executable

func (c *PlanCmd) Run() error {
	const op = "infra plan"
	abs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(abs))
	view, err := core.DescribeInfraAt(op, c.Console, c.LockPath, false, "")
	if err != nil {
		return err
	}
	env, err := FindEnv(view, c.Env)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !c.NoCodegen {
		self, err := planSelf()
		if err != nil {
			return err
		}
		args := []string{"codegen", "--force"}
		if c.Console != "" {
			args = append(args, "--console", c.Console)
		}
		fmt.Fprintf(core.Stdout, "── render deploy/%s for this tree\n", c.Env)
		if err := planRun(nil, core.Stdout, root, self, args...); err != nil {
			return fmt.Errorf("%s: codegen: %w", op, err)
		}
	}
	envDir := filepath.Join(root, "deploy", c.Env)
	if env.GetTargetKind() == "swarm" {
		ssh := env.GetHost().GetSsh()
		if ssh == "" {
			return fmt.Errorf("%s: environment %s names no server — `w17ctl infra set targets.%s.ssh=root@<server>`", op, c.Env, env.GetTarget())
		}
		t, err := remote.Parse(ssh)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return planSwarm(t, c.Env, envDir, root)
	}
	return planCloud(c.Env, env.GetTargetKind(), env.GetCloud().GetRegion(), envDir, root, c.StateBucket)
}

// planSwarm compares the rendered stack with the server's, file by file.
func planSwarm(t remote.Target, env, envDir, root string) error {
	stack := "/opt/stack/" + env
	srcs := append([]string(nil), remote.StackFiles...)
	if st, err := os.Stat(filepath.Join(envDir, "hooks")); err == nil && st.IsDir() {
		srcs = append(srcs, "hooks")
	}
	args := append([]string{"-anc", "--delete", "--itemize-changes", "--exclude", ".env*"}, t.RsyncArgs()...)
	args = append(append(args, srcs...), t.Dest()+":"+stack+"/")
	var out bytes.Buffer
	if err := planRun(nil, &out, envDir, "rsync", args...); err != nil {
		return fmt.Errorf("infra plan: comparing with %s: %w", t.Dest(), err)
	}
	var added, changed, removed []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "*deleting "):
			removed = append(removed, strings.TrimSpace(strings.TrimPrefix(line, "*deleting ")))
		case len(line) > 12 && line[0] == '<' && line[1] == 'f':
			p := strings.TrimSpace(line[12:])
			if strings.HasPrefix(line[2:11], "+++++++") {
				added = append(added, p)
			} else {
				changed = append(changed, p)
			}
		}
	}
	w := core.Stdout
	fmt.Fprintf(w, "\n── deploy/%s against %s:%s\n", env, t.Dest(), stack)
	if len(added)+len(changed)+len(removed) == 0 {
		fmt.Fprintln(w, "files: no change")
	}
	for _, p := range added {
		fmt.Fprintf(w, "+ %s\n", p)
	}
	for _, p := range removed {
		fmt.Fprintf(w, "- %s\n", p)
	}
	for _, p := range changed {
		fmt.Fprintf(w, "~ %s\n", p)
		var theirs bytes.Buffer
		if err := t.Run(nil, &theirs, t.Sudo("cat "+remote.ShellQuote(stack+"/"+p))); err != nil {
			return fmt.Errorf("infra plan: reading %s on the server: %w", p, err)
		}
		ours, err := os.ReadFile(filepath.Join(envDir, p)) // #nosec G304 -- the project's own rendered file
		if err != nil {
			return err
		}
		fmt.Fprint(w, lineDiff(theirs.String(), string(ours)))
	}
	var images bytes.Buffer
	if err := t.Run(nil, &images, t.Sudo("docker service ls --format '{{.Name}} {{.Image}}'")); err != nil {
		return fmt.Errorf("infra plan: listing the services on %s: %w", t.Dest(), err)
	}
	running := map[string]bool{}
	for _, l := range strings.Split(images.String(), "\n") {
		name, image, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok || !(strings.HasPrefix(name, env+"-blue_") || strings.HasPrefix(name, env+"-green_")) {
			continue
		}
		image, _, _ = strings.Cut(image, "@") // the digest Swarm pins
		if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
			running[image[i+1:]] = true
		}
	}
	var tags []string
	for tag := range running {
		tags = append(tags, tag)
	}
	var head bytes.Buffer
	_ = planRun(nil, &head, root, "git", "rev-parse", "--short=7", "HEAD")
	fmt.Fprintf(w, "images: running %s; this commit deploys sha-%s\n", orNoneList(tags), strings.TrimSpace(head.String()))
	fp, err := t.Fingerprint(stack)
	if err != nil {
		return fmt.Errorf("infra plan: %w", err)
	}
	fmt.Fprintf(w, "planned against %s — deploy exactly this with: w17ctl deploy --env %s --expect %s\n", fp, env, fp)
	return nil
}

func orNoneList(xs []string) string {
	if len(xs) == 0 {
		return "nothing yet"
	}
	return strings.Join(xs, ", ")
}

// planCloud runs `terraform plan` for this commit in the pinned Terraform
// image, as the invoking user, with the operator's own credentials.
func planCloud(env, kind, region, envDir, root, bucket string) error {
	if bucket == "" {
		return fmt.Errorf("infra plan: the Terraform state bucket — --state-bucket or TF_STATE_BUCKET (deploy/%s/terraform/bootstrap created it)", env)
	}
	tfDir := filepath.Join(envDir, "terraform")
	if _, err := os.Stat(filepath.Join(tfDir, "variables.tf")); err != nil {
		return fmt.Errorf("infra plan: %s has no Terraform — run `w17ctl codegen`", tfDir)
	}
	var head bytes.Buffer
	if err := planRun(nil, &head, root, "git", "rev-parse", "--short=7", "HEAD"); err != nil {
		return errors.New("infra plan: not a git repository with a commit — the images are tagged by the commit")
	}
	tag := "sha-" + strings.TrimSpace(head.String())
	base, err := terraformDocker(kind, tfDir)
	if err != nil {
		return err
	}
	backend := []string{"-backend-config=bucket=" + bucket}
	if kind == "aws-ecs" && region != "" {
		// The state bucket lives in the target's region, as deploy.sh says it.
		backend = append(backend, "-backend-config=region="+region)
	}
	fmt.Fprintf(core.Stdout, "\n── terraform plan of deploy/%s (%s), image %s\n", env, kind, tag)
	if err := planRun(nil, core.Stdout, root, "docker", append(append(base, "init", "-input=false"), backend...)...); err != nil {
		return fmt.Errorf("infra plan: terraform init: %w", err)
	}
	if err := planRun(nil, core.Stdout, root, "docker", append(base, "plan", "-input=false", "-var", "image_tag="+tag, "-var", "migrate_image_tag="+tag)...); err != nil {
		return fmt.Errorf("infra plan: terraform plan: %w", err)
	}
	return nil
}

// TerraformImage is the image every Terraform w17ctl runs comes from — the
// version the generated workflows install and the repository's checks use.
const TerraformImage = "hashicorp/terraform:1.16.5"

// terraformDocker is the `docker run …` prefix of a Terraform command in
// tfDir: the invoking user (the .terraform it writes stays theirs), the
// cloud credentials of the environment passed through (core.CloudCredentialArgs).
func terraformDocker(kind, tfDir string) ([]string, error) {
	if kind != "aws-ecs" && kind != "gcp-cloudrun" {
		return nil, fmt.Errorf("infra plan: no Terraform for target kind %q", kind)
	}
	args := []string{"run", "--rm", "-v", tfDir + ":/work", "-w", "/work", "-e", "HOME=/tmp", "-e", "TF_IN_AUTOMATION=1"}
	if u := os.Getuid(); u >= 0 {
		args = append(args, "-u", fmt.Sprintf("%d:%d", u, os.Getgid()))
	}
	args = append(args, core.CloudCredentialArgs(kind, "/tmp")...)
	return append(args, TerraformImage), nil
}

// lineDiff renders the lines that differ between a and b — `-` the server's,
// `+` this tree's — with two lines of context, from their longest common
// subsequence (rendered config files are small).
func lineDiff(a, b string) string {
	x := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
	y := strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			ops = append(ops, op{' ', x[i]})
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			// The server's line first, as a unified diff reads.
			ops = append(ops, op{'-', x[i]})
			i++
		default:
			ops = append(ops, op{'+', y[j]})
			j++
		}
	}
	const context = 2
	var out strings.Builder
	last := -1
	for k, o := range ops {
		if o.kind == ' ' {
			continue
		}
		from := max(k-context, last+1)
		if last >= 0 && from > last+1 {
			out.WriteString("    …\n")
		}
		for c := from; c < k; c++ {
			fmt.Fprintf(&out, "    %s\n", ops[c].text)
		}
		fmt.Fprintf(&out, "  %c %s\n", o.kind, o.text)
		last = k
		for c := k + 1; c < len(ops) && c <= k+context && ops[c].kind == ' '; c++ {
			fmt.Fprintf(&out, "    %s\n", ops[c].text)
			last = c
		}
	}
	return out.String()
}
