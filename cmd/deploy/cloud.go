package deploy

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// deployCloud deploys an aws-ecs / gcp-cloudrun environment the way its
// deploy workflow does, from this machine: the images built here and pushed
// to the environment's own registry, then deploy/<env>/ci/deploy.sh — the
// same script the workflow runs — in the generated deployer image
// (Terraform + the cloud's CLI), with the operator's own credentials and the
// values fetched from the console.
func (c *Cmd) deployCloud(root, envDir, kind string, env *codegenpb.LockInfraEnvironment, view *codegenpb.LockView, valuesDir string, sha string, dirty bool) error {
	ciDir := filepath.Join(envDir, "ci")
	if _, err := os.Stat(filepath.Join(ciDir, "deploy.sh")); err != nil {
		return fmt.Errorf("deploy: %s/deploy.sh is missing — run `w17ctl codegen`", ciDir)
	}
	region, account := env.GetCloud().GetRegion(), env.GetCloud().GetAccount()
	tag := "sha-" + sha
	if dirty {
		// The registries' tags are immutable: a second dirty deploy of one
		// commit needs a tag of its own.
		tag += "-dirty-" + strconv.FormatInt(time.Now().Unix(), 10)
	}

	step("the deployer image (Terraform + the cloud CLI)")
	deployer := "w17local/" + imageName(view.GetProject()) + "-deployer:" + c.Env
	if err := run(nil, core.Stdout, root, "docker", "build", "-q", "-t", deployer, "-f", filepath.Join(ciDir, "deployer.Dockerfile"), ciDir); err != nil {
		return fmt.Errorf("deploy: building the deployer: %w", err)
	}
	in := func(stdout io.Writer, script string) error {
		args := append([]string{"run", "--rm", "-e", "HOME=/tmp", "-e", "AWS_REGION=" + region}, core.CloudCredentialArgs(kind, "/creds")...)
		args = append(args, deployer, "bash", "-c", copyCLIConfig+script)
		return run(nil, stdout, root, "docker", args...)
	}

	step("log in to the environment's registry")
	var registry, prefix string
	var token bytes.Buffer
	if kind == "aws-ecs" {
		if account == "" {
			var out bytes.Buffer
			if err := in(&out, "aws sts get-caller-identity --query Account --output text"); err != nil {
				return fmt.Errorf("deploy: which AWS account: %w", err)
			}
			account = strings.TrimSpace(out.String())
		}
		registry = account + ".dkr.ecr." + region + ".amazonaws.com"
		prefix = registry + "/w17/" + c.Env
		if err := in(&token, "aws ecr get-login-password --region "+region); err != nil {
			return fmt.Errorf("deploy: ECR login: %w", err)
		}
		if err := run(&token, io.Discard, root, "docker", "login", "--username", "AWS", "--password-stdin", registry); err != nil {
			return fmt.Errorf("deploy: docker login %s: %w", registry, err)
		}
	} else {
		registry = region + "-docker.pkg.dev"
		prefix = registry + "/" + account + "/w17-" + c.Env
		if err := in(&token, "gcloud auth print-access-token"); err != nil {
			return fmt.Errorf("deploy: Artifact Registry token: %w", err)
		}
		if err := run(&token, io.Discard, root, "docker", "login", "--username", "oauth2accesstoken", "--password-stdin", "https://"+registry); err != nil {
			return fmt.Errorf("deploy: docker login %s: %w", registry, err)
		}
	}

	images, err := readImages(filepath.Join(ciDir, "images.txt"))
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	platform := firstPlatform(view.GetInfra().GetCi().GetPlatforms())
	for _, im := range images {
		ref := prefix + "/" + im.name + ":" + tag
		step("image " + im.name + " → " + ref)
		args := append([]string{"build", "-q", "--platform", platform}, c.BuildFlags...)
		for _, a := range im.buildArgs {
			args = append(args, "--build-arg", a)
		}
		if im.target != "" {
			args = append(args, "--target", im.target)
		}
		built := ref
		if im.lock {
			built = "w17local/" + imageName(view.GetProject()) + "/" + im.name + ":" + tag
		}
		args = append(args, "-t", built, "-f", im.dockerfile, im.context)
		if err := run(nil, core.Stdout, root, "docker", args...); err != nil {
			return fmt.Errorf("deploy: build %s: %w", im.name, err)
		}
		if im.lock {
			// The one-off migrate job reads the lock and the fixtures from the
			// image — the same layer the workflow bakes on.
			layer := "FROM " + built + "\nCOPY w17/lock.yaml /w17/lock.yaml\n"
			if st, err := os.Stat(filepath.Join(root, "w17", "fixtures")); err == nil && st.IsDir() {
				layer += "COPY w17/fixtures /w17/fixtures\n"
			}
			if err := run(strings.NewReader(layer), core.Stdout, root, "docker", "build", "-q", "--platform", platform, "-t", ref, "-f", "-", "."); err != nil {
				return fmt.Errorf("deploy: the lock layer of %s: %w", im.name, err)
			}
		}
		if err := run(nil, core.Stdout, root, "docker", "push", "-q", ref); err != nil {
			return fmt.Errorf("deploy: push %s: %w", ref, err)
		}
	}

	step("deploy/" + c.Env + "/ci/deploy.sh — " + kind)
	args := []string{"run", "--rm", "-v", root + ":/work", "-w", "/work", "-v", valuesDir + ":/values:ro",
		"-e", "HOME=/tmp", "-e", "VALUES_DIR=/values", "-e", "TAG=" + tag, "-e", "TF_STATE_BUCKET=" + c.StateBucket, "-e", "AWS_REGION=" + region}
	if u := os.Getuid(); u >= 0 {
		// The .terraform deploy.sh writes into deploy/<env>/terraform stays yours.
		args = append(args, "-u", fmt.Sprintf("%d:%d", u, os.Getgid()))
	}
	args = append(args, core.CloudCredentialArgs(kind, "/creds")...)
	args = append(args, deployer, "bash", "-c", copyCLIConfig+"exec bash deploy/"+c.Env+"/ci/deploy.sh")
	if err := run(nil, core.Stdout, root, "docker", args...); err != nil {
		return fmt.Errorf("deploy: deploy.sh: %w", err)
	}
	fmt.Fprintf(core.Stdout, "\ndeployed %s (%s) to %s — commit the lock: migrate generate pinned new migrations in it.\n", tag, c.Env, kind)
	fmt.Fprint(core.Stdout, warning)
	return nil
}

// copyCLIConfig copies the read-only mounted CLI configurations into the
// container's $HOME: the CLIs write caches and tokens beside them.
const copyCLIConfig = `for d in .aws .config/gcloud; do [ ! -d "/creds/$d" ] || { mkdir -p "$HOME/$(dirname "$d")" && cp -r "/creds/$d" "$HOME/$d"; }; done; `

// firstPlatform is the platform a local build targets: the CI block's first
// (the clouds run linux/amd64 unless told otherwise).
func firstPlatform(platforms string) string {
	if p, _, _ := strings.Cut(platforms, ","); p != "" {
		return p
	}
	return "linux/amd64"
}
