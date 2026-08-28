// Package docker holds the local docker-compose shell-out primitives the
// stack / project / migrate commands drive. Seams (RunComposeFn /
// RunComposeEnvFn) let tests stub the shell-out.
package docker

import (
	"bytes"
	"io"
	"os"
	"os/exec"

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
