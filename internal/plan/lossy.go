package plan

import (
	"fmt"
	"sort"
	"strings"

	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// What to do when a sync would destroy something.
//
// The three answers are all legitimate, which is why this is a choice and not
// a rule. A developer who just renamed a field wants the drop. A developer who
// has been typing test data into that table for an hour does not. And a third
// wants both — the change applied, and what was there kept where it can be put
// back.
const (
	// LossyRefuse stops before applying anything and names what would be
	// lost. The default, because it is the only one of the three that is
	// recoverable from: the other two have already happened by the time the
	// developer reads the output.
	LossyRefuse = "refuse"
	// LossyApply goes ahead.
	LossyApply = "apply"
	// LossySnapshot snapshots the affected stores first, then goes ahead.
	LossySnapshot = "snapshot"
)

// ValidLossyModes lists the accepted values, for a flag's help and its
// refusal.
func ValidLossyModes() string {
	return strings.Join([]string{LossyRefuse, LossyApply, LossySnapshot}, " | ")
}

// LossyRefusal renders the refusal a caller prints when a plan would destroy
// something and the mode says not to. Empty when there is nothing to lose.
//
// It names every loss, because "this change is destructive" is not something a
// person can act on — which table, which column, and what it becomes are.
func LossyRefusal(lossy []*codegenpb.LossyChange) string {
	if len(lossy) == 0 {
		return ""
	}
	byConn := map[string][]string{}
	for _, l := range lossy {
		line := "    " + describeLoss(l)
		byConn[l.GetConnection()] = append(byConn[l.GetConnection()], line)
	}
	conns := make([]string, 0, len(byConn))
	for c := range byConn {
		conns = append(conns, c)
	}
	sort.Strings(conns)

	var b strings.Builder
	b.WriteString("this sync would DESTROY data in your local database\n")
	for _, c := range conns {
		name := c
		if name == "" {
			name = "(default connection)"
		}
		fmt.Fprintf(&b, "  %s:\n%s\n", name, strings.Join(byConn[c], "\n"))
	}
	b.WriteString("\n" +
		"  why: a dev sync makes the database match your protos, and matching them here\n" +
		"       means removing what they no longer describe. Refused rather than applied,\n" +
		"       because this is the one direction that cannot be undone by running it again.\n" +
		"  choose:\n" +
		"    --lossy=snapshot   snapshot these stores first, then apply (restore: w17ctl db snapshot restore)\n" +
		"    --lossy=apply      apply it; the data is gone\n" +
		"    (or change the protos back, and this disappears)")
	return b.String()
}

// describeLoss puts one change into a sentence a person can act on.
func describeLoss(l *codegenpb.LossyChange) string {
	switch l.GetKind() {
	case "drop_table":
		return "the table " + l.GetObject() + " is dropped, with every row in it"
	case "drop_column":
		return "the column " + l.GetObject() + " is dropped, with every value in it"
	case "retype_column":
		detail := l.GetDetail()
		if detail != "" {
			detail = " (" + detail + ")"
		}
		return "the column " + l.GetObject() + " is retyped" + detail +
			", which rewrites every value and can fail or truncate"
	default:
		return l.GetKind() + " " + l.GetObject()
	}
}

// LossyConnections lists, in a stable order, the connections a plan would
// destroy something in — the stores a snapshot has to cover.
func LossyConnections(lossy []*codegenpb.LossyChange) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lossy {
		if !seen[l.GetConnection()] {
			seen[l.GetConnection()] = true
			out = append(out, l.GetConnection())
		}
	}
	sort.Strings(out)
	return out
}
