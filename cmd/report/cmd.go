// Package report is `w17ctl report`: tell w17 what is wrong, missing or
// unclear, and follow what happens to it. The report is kept and triaged by
// the console, not by a third-party tracker; this command only sends
// it and reads it back.
package report

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/sdkupdate"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	supportpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/support"
)

// The console's limits, held here too so a report that would be refused is
// refused before it is sent — and before an editor session is lost to it.
const (
	maxTitleRunes   = 200
	maxBodyBytes    = 256 << 10
	maxDiagnostics  = 4 << 10
	perAccountLimit = 16
	perOrgLimit     = 64
)

// Cmd is `w17ctl report`.
type Cmd struct {
	Security SecurityCmd `cmd:"" help:"Report a security issue — a vulnerability. Treated as critical until triage says otherwise."`
	Bug      BugCmd      `cmd:"" help:"Report a bug — something does not work. --critical when it blocks you."`
	Docs     DocsCmd     `cmd:"" help:"Report documentation that is wrong, missing or not enough for a function (--topic names it)."`
	Feature  FeatureCmd  `cmd:"" help:"Ask for something w17 does not do yet."`
	List     ListCmd     `cmd:"" help:"List your organization's reports and where each stands."`
	Show     ShowCmd     `cmd:"" help:"Show one report: what was sent and what triage answered."`
	Withdraw WithdrawCmd `cmd:"" help:"Take back one of your own reports while it is open or triaged."`
}

// Help is shown under `w17ctl report --help`.
func (Cmd) Help() string {
	return fmt.Sprintf(`A report goes to w17's console and is triaged there; nothing goes to a
third party. The body is Markdown, as long as it needs to be (at most 256 KiB):
what you did, what happened, what you expected, how to reproduce it.

Each account files at most %d reports a day and each organization %d (UTC).
You are mailed when your report ships in a release, and when triage makes it
later than you would expect (a severity below critical, a feature planned for
later, a close) — not otherwise. --no-notify turns that off for one report.`, perAccountLimit, perOrgLimit)
}

// One subcommand per kind, each the same flags.
type (
	SecurityCmd struct {
		FileCmd `embed:""`
	}
	BugCmd struct {
		FileCmd `embed:""`
	}
	DocsCmd struct {
		FileCmd `embed:""`
	}
	FeatureCmd struct {
		FileCmd `embed:""`
	}
)

func (c *SecurityCmd) Run() error { return c.file(supportpb.IssueReport_SECURITY) }
func (c *BugCmd) Run() error      { return c.file(supportpb.IssueReport_BUG) }
func (c *DocsCmd) Run() error     { return c.file(supportpb.IssueReport_DOCS) }
func (c *FeatureCmd) Run() error  { return c.file(supportpb.IssueReport_FEATURE) }

// FileCmd is what filing a report takes, whatever its kind.
type FileCmd struct {
	Title         string `name:"title" short:"t" help:"One line, at most 200 characters. Asked for when omitted on a terminal."`
	BodyFile      string `name:"body-file" short:"F" placeholder:"PATH" help:"Read the body (Markdown) from a file; - is stdin. Without it, stdin when it is not a terminal, else $EDITOR."`
	Topic         string `name:"topic" placeholder:"NAME" help:"docs: the function, command or page the documentation is about (added to the title)."`
	Critical      bool   `name:"critical" help:"bug: it blocks you. Triage may set it lower, and then tells you."`
	NoNotify      bool   `name:"no-notify" help:"Do not mail me about this report — not when it ships, not when triage makes it later."`
	NoDiagnostics bool   `name:"no-diagnostics" help:"Do not attach this w17ctl's version, the OS and the project's codegen version."`
	Console       string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console endpoint. Defaults to the logged-in console."`
}

// The seams the tests replace.
var (
	stdin      io.Reader = os.Stdin
	stdinIsTTY           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	runEditor            = editInEditor
)

func (c *FileCmd) file(kind supportpb.IssueReport_Kind) error {
	switch {
	case c.Critical && kind != supportpb.IssueReport_BUG:
		return errors.New("report: --critical is for a bug (a security report is critical already)")
	case c.Topic != "" && kind != supportpb.IssueReport_DOCS:
		return errors.New("report: --topic is for a docs report")
	}
	tty := stdinIsTTY()
	title := strings.TrimSpace(c.Title)
	if title == "" && tty {
		t, err := askLine("Title (one line): ")
		if err != nil {
			return fmt.Errorf("report: %w", err)
		}
		title = t
	}
	if c.Topic != "" {
		title = strings.TrimSpace(c.Topic) + ": " + title
	}
	if err := checkTitle(title); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	body, err := c.body(kind, title, tty)
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	diag := ""
	if !c.NoDiagnostics {
		diag = diagnostics()
	}
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	cctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.IR.SubmitIssueReport(cctx, &supportpb.SubmitIssueReportReq{
		Kind:        kind,
		Title:       title,
		Body:        body,
		Diagnostics: diag,
		Critical:    c.Critical,
		NoNotify:    c.NoNotify,
	})
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	fmt.Fprintf(core.Stdout, "filed %s report %s\n", kindName(kind), resp.GetId())
	fmt.Fprintf(core.Stdout, "follow it with `w17ctl report show %s`; you can file %d more today\n", resp.GetId(), resp.GetRemainingToday())
	return nil
}

// body is the report's text: from --body-file, from stdin when it is a pipe,
// or written in $EDITOR on a terminal.
func (c *FileCmd) body(kind supportpb.IssueReport_Kind, title string, tty bool) (string, error) {
	var b []byte
	var err error
	switch {
	case c.BodyFile == "-" || (c.BodyFile == "" && !tty):
		b, err = io.ReadAll(io.LimitReader(stdin, maxBodyBytes+1))
	case c.BodyFile != "":
		b, err = readLimited(c.BodyFile)
	default:
		var text string
		text, err = runEditor(template(kind, title))
		b = []byte(stripTemplate(text))
	}
	if err != nil {
		return "", err
	}
	return string(b), checkBody(string(b))
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	return io.ReadAll(io.LimitReader(f, maxBodyBytes+1))
}

func checkTitle(t string) error {
	switch {
	case t == "":
		return errors.New("a report needs a title (--title)")
	case strings.ContainsAny(t, "\r\n"):
		return errors.New("the title is one line")
	case utf8.RuneCountInString(t) > maxTitleRunes:
		return fmt.Errorf("the title is at most %d characters", maxTitleRunes)
	}
	return nil
}

func checkBody(b string) error {
	switch {
	case strings.TrimSpace(b) == "":
		return errors.New("the report is empty: say what happened, what you expected and how to reproduce it")
	case len(b) > maxBodyBytes:
		return fmt.Errorf("the body is over %d bytes (256 KiB) — trim logs to what shows the problem", maxBodyBytes)
	case !utf8.ValidString(b):
		return errors.New("the body must be UTF-8 text")
	}
	return nil
}

// templateMarker starts the lines an editor template carries for the writer
// and the report leaves out.
const templateMarker = "#~ "

func template(kind supportpb.IssueReport_Kind, title string) string {
	var hint string
	switch kind {
	case supportpb.IssueReport_SECURITY:
		hint = "What is exposed, to whom, and how. Steps to reproduce; the versions involved."
	case supportpb.IssueReport_BUG:
		hint = "What you did, what happened, what you expected. Steps to reproduce; logs that show it."
	case supportpb.IssueReport_DOCS:
		hint = "Which page or function, what is wrong or missing, and what you needed to know."
	case supportpb.IssueReport_FEATURE:
		hint = "What you want to do, why, and what you do today instead."
	}
	return templateMarker + "Report: " + title + "\n" +
		templateMarker + hint + "\n" +
		templateMarker + "Markdown. Lines starting with '" + strings.TrimSpace(templateMarker) + "' are left out. Save and quit to send; an empty body sends nothing.\n\n"
}

func stripTemplate(text string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, strings.TrimSpace(templateMarker)) {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n"
}

// editInEditor opens $VISUAL / $EDITOR (vi) on a temporary file holding
// initial, and returns what was saved.
func editInEditor(initial string) (string, error) {
	f, err := os.CreateTemp("", "w17-report-*.md")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }() // a leftover temp file is the editor's draft, nothing more
	if _, err := f.WriteString(initial); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], f.Name())...) //nolint:gosec // G204: the user's own $EDITOR on a file we made
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor %s: %w", parts[0], err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	if len(b) > maxBodyBytes+4096 {
		return "", fmt.Errorf("the body is over %d bytes (256 KiB)", maxBodyBytes)
	}
	return string(b), nil
}

func askLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// diagnostics is what the report says about where it was filed from: this
// w17ctl's version, the OS, and the project's codegen version when run in a
// project. Nothing else — no paths, no files, no environment.
func diagnostics() string {
	lines := []string{
		"w17ctl: " + core.VersionString(),
		"os: " + runtime.GOOS + "/" + runtime.GOARCH,
	}
	if root, err := core.FindProjectRoot(); err == nil {
		if v := sdkupdate.LockedCodegenVersion(root); v != "" {
			lines = append(lines, "codegen_version: "+v)
		}
		if v := sdkupdate.LockedVersion(filepath.Join(root, "w17", "lock.yaml")); v != "" {
			lines = append(lines, "sdk_version: "+v)
		}
	}
	d := strings.Join(lines, "\n")
	if len(d) > maxDiagnostics {
		d = d[:maxDiagnostics]
	}
	return d
}

// ListCmd is `w17ctl report list`.
type ListCmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console endpoint. Defaults to the logged-in console."`
}

func (c *ListCmd) Run() error {
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.IR.ListIssueReports(ctx, &supportpb.ListOrgIssueReportsReq{})
	if err != nil {
		return fmt.Errorf("report list: %w", err)
	}
	if len(resp.GetItems()) == 0 {
		fmt.Fprintln(core.Stdout, "no reports in this organization")
		return nil
	}
	for _, r := range resp.GetItems() {
		fmt.Fprintf(core.Stdout, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.GetId(), kindName(r.GetKind()), standing(r.GetStatus(), r.GetSeverity(), r.GetHorizon(), r.GetResolution(), r.GetReleasedIn()),
			r.GetCreatedAt().AsTime().UTC().Format("2006-01-02"), r.GetAuthorEmail(), r.GetTitle())
	}
	return nil
}

// ShowCmd is `w17ctl report show ID`.
type ShowCmd struct {
	ID      string `arg:"" name:"id" help:"The report's id (report list)."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console endpoint. Defaults to the logged-in console."`
}

func (c *ShowCmd) Run() error {
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	r, err := sc.IR.GetIssueReport(ctx, &supportpb.GetOrgIssueReportReq{Id: c.ID})
	if err != nil {
		return fmt.Errorf("report show: %w", err)
	}
	w := core.Stdout
	fmt.Fprintf(w, "%s report %s — %s\n", kindName(r.GetKind()), r.GetId(), r.GetTitle())
	fmt.Fprintf(w, "status:   %s\n", standing(r.GetStatus(), r.GetSeverity(), r.GetHorizon(), r.GetResolution(), r.GetReleasedIn()))
	fmt.Fprintf(w, "filed:    %s by %s\n", r.GetCreatedAt().AsTime().UTC().Format("2006-01-02 15:04 UTC"), r.GetAuthorEmail())
	if !r.GetNotify() {
		fmt.Fprintln(w, "mail:     off for this report")
	}
	if n := strings.TrimSpace(r.GetPublicNote()); n != "" {
		fmt.Fprintf(w, "\ntriage:\n%s\n", n)
	}
	fmt.Fprintf(w, "\n%s\n", strings.TrimRight(r.GetBody(), "\n"))
	return nil
}

// WithdrawCmd is `w17ctl report withdraw ID`.
type WithdrawCmd struct {
	ID      string `arg:"" name:"id" help:"The report's id (report list)."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console endpoint. Defaults to the logged-in console."`
}

func (c *WithdrawCmd) Run() error {
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	if _, err := sc.IR.WithdrawIssueReport(ctx, &supportpb.WithdrawReportReq{Id: c.ID}); err != nil {
		return fmt.Errorf("report withdraw: %w", err)
	}
	fmt.Fprintf(core.Stdout, "withdrew report %s\n", c.ID)
	return nil
}

func kindName(k supportpb.IssueReport_Kind) string {
	switch k {
	case supportpb.IssueReport_SECURITY:
		return "security"
	case supportpb.IssueReport_BUG:
		return "bug"
	case supportpb.IssueReport_DOCS:
		return "docs"
	case supportpb.IssueReport_FEATURE:
		return "feature"
	}
	return "report"
}

// standing is where a report stands, in one phrase.
func standing(s supportpb.IssueReport_Status, sev supportpb.IssueReport_Severity, h supportpb.IssueReport_Horizon, res supportpb.IssueReport_Resolution, release string) string {
	lower := func(x fmt.Stringer) string { return strings.ToLower(strings.ReplaceAll(x.String(), "_", " ")) }
	switch s {
	case supportpb.IssueReport_OPEN:
		return "open, waiting for triage"
	case supportpb.IssueReport_TRIAGED:
		if h != supportpb.IssueReport_HORIZON_UNSPECIFIED {
			return "planned, " + lower(h)
		}
		return "triaged, " + lower(sev)
	case supportpb.IssueReport_RESOLVED:
		return "fixed, waiting for a release"
	case supportpb.IssueReport_RELEASED:
		return "released in " + release
	case supportpb.IssueReport_CLOSED:
		return "closed, " + lower(res)
	case supportpb.IssueReport_WITHDRAWN:
		return "withdrawn"
	}
	return lower(s)
}
