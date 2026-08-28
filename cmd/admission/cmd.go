// Package admission wires `w17ctl admission` — a read of the console's
// codegen admission-gate counters.
//
// The gate bounds concurrent codegen against the daemon's real memory budget
// and logs each non-trivial decision. Logs answer "what happened to that
// request"; they cannot answer "how many has it turned away", "how deep is the
// queue right now", or "how far has the cost model drifted" — those are
// running state, and this is the surface that reads them.
//
// It is a console read, not a project operation: it takes no lock, no proto
// tree, and no project root. The counters are PROCESS-LOCAL to the daemon
// doing the generating, so the number only means anything from there.
package admission

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default."`
}

func (c *Cmd) Run() error {
	client, conn, err := core.DialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("admission: dial console: %w", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := client.AdmissionStatus(ctx, &codegenpb.AdmissionStatusRequest{})
	if err != nil {
		return fmt.Errorf("admission: %w", err)
	}

	// `enabled` first and on its own line: every number below is meaningless
	// when the gate is off, and a table of zeros reads as "nothing rejected".
	if !st.GetEnabled() {
		fmt.Fprintln(core.Stdout, "codegen admission: OFF — the numbers below are not a measurement of a working gate.")
	} else {
		fmt.Fprintf(core.Stdout, "codegen admission: ON  (queue=%d, max_lines=%d)\n", st.GetMaxQueueSize(), st.GetMaxLines())
	}
	fmt.Fprintf(core.Stdout, "  in flight   %d\n  waiting     %d\n", st.GetInFlight(), st.GetWaiting())

	dec := st.GetDecisions()
	if len(dec) == 0 {
		fmt.Fprintln(core.Stdout, "  decisions   (none yet — this daemon has admitted nothing since it started)")
	} else {
		keys := make([]string, 0, len(dec))
		for k := range dec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(core.Stdout, "  decisions")
		for _, k := range keys {
			fmt.Fprintf(core.Stdout, "    %-22s %d\n", k, dec[k])
		}
	}

	fmt.Fprintf(core.Stdout, "  wait        total %s, max %s\n",
		time.Duration(st.GetWaitTotalMs())*time.Millisecond,
		time.Duration(st.GetWaitMaxMs())*time.Millisecond)

	// The calibration line. A mean creeping toward 1 — or any under-estimate
	// at all — is what says the cost function in codegen_admission.go needs
	// re-fitting; it is the reason these counters exist and the one number a
	// log line cannot accumulate.
	if runs := st.GetRuns(); runs == 0 {
		fmt.Fprintln(core.Stdout, "  estimate    (no completed runs to compare against)")
	} else {
		fmt.Fprintf(core.Stdout, "  estimate    %d run(s), observed/estimate mean %.2f max %.2f, %d under-estimate(s)\n",
			runs, st.GetRatioSum()/float64(runs), st.GetRatioMax(), st.GetUnderEstimates())
	}
	return nil
}
