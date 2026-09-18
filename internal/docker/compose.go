// Package docker holds the local docker-compose shell-out primitives the
// stack / project / migrate commands drive. Seams (RunComposeFn /
// RunComposeEnvFn) let tests stub the shell-out.
package docker

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// RunComposeFn is the seam tests stub; production runs realRunCompose.
var RunComposeFn = realRunCompose

func realRunCompose(dir string, args ...string) error {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.Stdout = core.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunComposeEnvFn is the seam tests stub; production runs realRunComposeEnv.
var RunComposeEnvFn = realRunComposeEnv

func realRunComposeEnv(dir string, env []string, args ...string) error {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = core.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CaptureComposeFn is the seam tests stub; production runs
// realCaptureCompose. It captures a compose subcommand's stdout (e.g.
// `ps --format json`) for programmatic inspection rather than streaming
// it to the terminal. Stderr is discarded — callers treat any error as
// "no data" and degrade gracefully.
var CaptureComposeFn = realCaptureCompose

func realCaptureCompose(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return buf.Bytes(), err
}

// ComposeFile is the tool-owned compose file. The sibling `compose.yaml` is
// the developer's extension point and `include:`s this one.
const ComposeFile = "compose.w17.yaml"

// stubCompose is the conventional stub w17 writes beside ComposeFile when the
// project has none.
const stubCompose = "compose.yaml"

// FileArgs returns the `-f <file>` docker compose should be driven with for
// the project at root.
//
// Without it, compose picks its own default — `compose.yaml` in the working
// directory — and in a repo that had one BEFORE w17 arrived, that is somebody
// else's file. A consumer's `stack up` started building their unrelated
// services and failed pulling an image w17 never heard of. Two correct
// decisions produced it: codegen refuses to overwrite an existing
// compose.yaml, and the stack is driven from the project root. Neither is
// wrong; the intersection had no way to say "just mine".
//
// So the choice is made here rather than left to discovery:
//
//   - compose.yaml INCLUDES ComposeFile → it is the stub (or a developer's
//     extension of it), and driving it is what brings both in;
//   - otherwise → ComposeFile alone. A compose.yaml that does not include
//     ours is not ours to start.
//
// For a greenfield project, where codegen wrote the stub itself, this is the
// behaviour that was already there.
func FileArgs(root string) []string {
	if includesW17Compose(filepath.Join(root, stubCompose)) {
		return []string{"-f", stubCompose}
	}
	return []string{"-f", ComposeFile}
}

// includesW17Compose reports whether the compose file at path pulls in
// ComposeFile. Deliberately a text scan rather than a YAML parse: the
// question is "does this file reference ours", the answer must not depend on
// the file elsewhere being valid, and a malformed compose.yaml belonging to
// someone else must not stop w17 from starting its own.
//
// Comments are cut first, and that is not tidiness. A brownfield repo whose
// own compose merely MENTIONS our file in a comment — "the w17 services live
// in compose.w17.yaml" — was read as including it, so every stack command
// drove the foreign file instead: `stack build <our service>` answered "no
// such service", and the port lookup that follows found nothing and said to
// run `stack up` first, about a database that was already running. A
// consumer reported both and guessed they shared a root. They did.
func includesW17Compose(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if code, _, _ := bytes.Cut(line, []byte("#")); bytes.Contains(code, []byte(ComposeFile)) {
			return true
		}
	}
	return false
}
