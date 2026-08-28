package testsuite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Stress drives the project's generated stress/load presets against an
// EXTERNAL, already-running target. Unlike [Config] (the e2e suite), it
// owns NO stack: the developer points it at a deployed gateway URL and it
// hammers that — bringing up an ephemeral stack + assigning a dynamic port
// only makes sense for the once-through assert run, not for load, where you
// want to measure a real deployment. It builds the SAME e2erunner host
// binary (cross-compiled in Docker — no local Go needed) and runs ONLY the
// stress presets (`-stress`, filtered to the generated Test_*_stress cases)
// against Target, so the e2e assertion scenarios never fire against a
// production-shaped endpoint.
//
// One run at a time by design: a stress run IS a single measured window
// (concurrency lives inside the plan). A series is a shell loop over
// distinct targets, not a flag here.
type Stress struct {
	Target      string // REST base URL of the gateway under load (required)
	Preset      string // run only this preset (empty = every preset)
	Domain      string // scope Preset to this domain (empty = any domain; disambiguates a preset name shared across domains)
	Concurrency int    // override every preset's worker count (0 = use the plan)
	Duration    string // override every preset's run window, e.g. 30s (empty = use the plan)
	Total       int    // override every preset's total_requests (0 = use the plan)
	Verbose     bool   // -test.v — per-preset framework output around the report
	Image       string // builder image used to cross-compile the runner
	GomodVolume string // named Docker volume for the Go module cache
	Console     string // console gRPC endpoint (owns the lock — read for the services dir)
}

// Run builds the e2erunner and fires the stress presets against Target.
// The generated presets print their own throughput/latency report to
// stdout; a threshold breach (or an unreachable target) fails the run with
// a non-zero exit, so `w17ctl stress` doubles as a CI load gate.
func (s *Stress) Run() error {
	if s.Target == "" {
		return fmt.Errorf("stress needs a target — the REST base URL of the deployed gateway to load (e.g. `w17ctl stress https://staging.example.com`)")
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	view, err := core.DescribeLockFromRoot(s.Console, root)
	if err != nil {
		return err
	}
	if view.GetE2EDir() == "" {
		return fmt.Errorf("e2e is not enabled for this project (w17/lock.yaml generated_code.e2e_dir is unset) — stress presets live under the e2e tree")
	}
	runnerDir := filepath.Join(root, filepath.FromSlash(view.GetServicesDir()), "e2erunner")
	if _, err := os.Stat(filepath.Join(runnerDir, "main_test.go")); err != nil {
		return fmt.Errorf("no generated e2erunner module at %s — run the project's codegen first", runnerDir)
	}
	if _, err := lookPath("docker"); err != nil {
		return fmt.Errorf("w17ctl stress cross-compiles the runner in Docker; `docker` was not found on PATH")
	}

	_ = runCmd(exec.Command("docker", "volume", "create", s.GomodVolume))

	bin := filepath.Join(root, "bin", "e2erunner")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "w17ctl stress: building the load runner")
	if err := buildRunnerBin(root, runnerDir, bin, s.Image, s.GomodVolume); err != nil {
		return fmt.Errorf("build e2erunner: %w", err)
	}

	fmt.Fprintf(os.Stderr, "w17ctl stress: loading %s\n", s.Target)
	return s.run(bin)
}

// testFilter is the -test.run regexp that narrows the run to the wanted
// generated stress cases (named Test_<domain>_<preset>_stress). With no
// --preset/--domain it selects every stress case (and only those — the e2e
// assertion scenarios never fire against a load target); --preset pins one
// preset (suffix-anchored so it can't spill into scenarios), and --domain
// scopes it (or, alone, a whole domain's presets).
func (s *Stress) testFilter() string {
	switch {
	case s.Domain != "" && s.Preset != "":
		return "^Test_" + regexp.QuoteMeta(s.Domain+"_"+s.Preset) + "_stress$"
	case s.Preset != "":
		return "_" + regexp.QuoteMeta(s.Preset) + "_stress$"
	case s.Domain != "":
		return "^Test_" + regexp.QuoteMeta(s.Domain) + "_.*_stress$"
	default:
		return "_stress$"
	}
}

// run fires the compiled runner in stress mode against the target. It
// filters to the generated Test_*_stress cases with -test.run so the e2e
// assertion scenarios (which mutate + assert) never run against the load
// target, and forwards the -stress.* plan overrides.
func (s *Stress) run(bin string) error {
	args := []string{"-target", s.Target, "-stress", "-test.run", s.testFilter()}
	if s.Verbose {
		args = append(args, "-test.v")
	}
	if s.Concurrency > 0 {
		args = append(args, "-stress.concurrency", fmt.Sprint(s.Concurrency))
	}
	if s.Duration != "" {
		args = append(args, "-stress.duration", s.Duration)
	}
	if s.Total > 0 {
		args = append(args, "-stress.total", fmt.Sprint(s.Total))
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := runCmd(cmd); err != nil {
		return fmt.Errorf("stress run failed (a threshold was breached or the target was unreachable)")
	}
	return nil
}
