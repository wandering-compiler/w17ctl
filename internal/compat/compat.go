package compat

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Compat is the console v2 compatibility engine — a COMPILER concern, so
// w17ctl (the thin client) classifies over the API rather than embedding
// lib/compat or even the IR type. The classifier + the dev/prod policy gate
// run server-side on the console's CodegenService.Classify; the client ships
// two compiled IR states as OPAQUE bytes (a snapshot's ir_schema or a fresh
// CompileIR output) and renders the typed verdict (public-split boundary —
// zero compiler logic / IR struct on the client).

// ClassifyCompat ships two compiled IR states (opaque marshalled bytes — a
// snapshot's ir_schema or a fresh CompileIR output) to the console's
// CodegenService.Classify and returns the classification + gate decision for
// `mode`. base may be empty (initial state). It dials the same CodegenService
// endpoint as CompileIR (a leaf operation, not a hot path).
func ClassifyCompat(base, head []byte, mode string) (*codegenpb.ClassifyIRResponse, error) {
	addr, err := core.ResolveConsoleAddr("")
	if err != nil {
		return nil, err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := cl.Classify(ctx, &codegenpb.ClassifyIRRequest{Base: base, Head: head, Mode: mode})
	if err != nil {
		return nil, fmt.Errorf("compat classify: %w", err)
	}
	return resp, nil
}

// RenderReportPB prints the findings + the gate readout from a Classify
// response and returns whether the gate BLOCKS. Callers that are a gate
// (compat report, against-trunk, merge) turn a true into a non-zero exit;
// read-only callers (review show) ignore it.
func RenderReportPB(resp *codegenpb.ClassifyIRResponse, mode string) (blocked bool) {
	if len(resp.GetFindings()) == 0 {
		fmt.Fprintln(core.Stdout, "no changes (SAFE)")
		return false
	}
	for _, f := range resp.GetFindings() {
		fmt.Fprintf(core.Stdout, "%-4s %-8s %-26s %s\n", f.GetDomain(), f.GetSeverity(), f.GetChange(), f.GetMessage())
	}
	fmt.Fprintf(core.Stdout, "\nmax_severity: %s → %s mode: %s\n", resp.GetMaxSeverity(), mode, resp.GetAction())
	return resp.GetBlocked()
}

// DBBreakingFindings returns the DB-domain BREAKING findings of a Classify
// response — the subset that decides whether a dev DB must be wiped (a
// wire/API break alone never requires a DB reset). Domain/Severity are the
// pinned string contract ("DB"/"BREAKING").
func DBBreakingFindings(resp *codegenpb.ClassifyIRResponse) []*codegenpb.CompatFinding {
	var out []*codegenpb.CompatFinding
	for _, f := range resp.GetFindings() {
		if f.GetDomain() == "DB" && f.GetSeverity() == "BREAKING" {
			out = append(out, f)
		}
	}
	return out
}
