package prompter

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Prompter is the operator-facing input source for `w17ctl
// init` / `w17ctl connection add` wizards. Production wires
// the stdin-backed implementation; tests pump pre-baked
// answers through a slice stub. Every wizard step goes
// through this interface — no direct os.Stdin reads in cmd
// code.
type Prompter interface {
	// Text prints the question + optional default hint and
	// returns the operator's free-text answer. Empty input
	// returns the default; non-empty returns the trimmed
	// input.
	Text(question, defaultValue string) (string, error)

	// Select prints the question + a numbered list of options
	// and returns the chosen value. Default is one of the
	// options (or empty for "no default — operator must
	// pick"). Empty input with no default re-prompts.
	Select(question string, options []string, defaultValue string) (string, error)

	// Password prints the question and reads a secret WITHOUT
	// echoing it to the terminal (for passwords). When stdin is
	// not a terminal (piped input / tests) it falls back to a
	// normal buffered line read.
	Password(question string) (string, error)
}

// stdinPrompter is the production-path Prompter that reads
// answers from os.Stdin and writes prompts to os.Stdout.
// Tests use stubPrompter (in prompter_test.go).
type stdinPrompter struct {
	in   *bufio.Reader
	out  io.Writer
	file *os.File // underlying terminal for hidden (no-echo) reads
}

// NewStdinPrompter constructs the production Prompter wired
// to os.Stdin / os.Stdout. Cmd code calls this once per
// wizard run.
func NewStdinPrompter() Prompter {
	return &stdinPrompter{
		in:   bufio.NewReader(os.Stdin),
		out:  os.Stdout,
		file: os.Stdin,
	}
}

// Text prints "question [default]: " (or "question: " when
// no default), reads one line, trims whitespace, returns the
// answer or the default on empty input.
func (p *stdinPrompter) Text(question, defaultValue string) (string, error) {
	if defaultValue != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, defaultValue)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read input: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return defaultValue, nil
	}
	return answer, nil
}

// Select prints the numbered options + reads a 1-based index
// (or the literal value), maps to the chosen option. Empty
// input + non-empty default → return default; empty input +
// empty default → re-prompt with "please pick one".
func (p *stdinPrompter) Select(question string, options []string, defaultValue string) (string, error) {
	for {
		fmt.Fprintln(p.out, question)
		for i, opt := range options {
			marker := " "
			if opt == defaultValue {
				marker = "*"
			}
			fmt.Fprintf(p.out, "  %s %d) %s\n", marker, i+1, opt)
		}
		hint := "pick a number"
		if defaultValue != "" {
			hint = fmt.Sprintf("pick a number [%s]", defaultValue)
		}
		fmt.Fprintf(p.out, "%s: ", hint)
		line, err := p.in.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read input: %w", err)
		}
		answer := strings.TrimSpace(line)
		if answer == "" {
			if defaultValue != "" {
				return defaultValue, nil
			}
			if err == io.EOF {
				// stdin closed with no answer and no default to fall back
				// on. Re-prompting would spin forever (ReadString keeps
				// returning EOF) — fail terminally instead.
				return "", fmt.Errorf("no input: stdin closed before a selection was made")
			}
			fmt.Fprintln(p.out, "please pick one")
			continue
		}
		if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(options) {
			return options[idx-1], nil
		}
		// Allow literal-value answers too — operator types
		// "postgres" instead of the index. Accept iff the
		// value is in the option list.
		for _, opt := range options {
			if opt == answer {
				return opt, nil
			}
		}
		fmt.Fprintf(p.out, "invalid choice %q; expected a number 1-%d or one of the listed values\n",
			answer, len(options))
	}
}

// Password prints "question: " and reads a line without echoing it when
// stdin is a terminal (term.ReadPassword on the fd). ReadPassword consumes
// the trailing Enter without printing a newline, so we emit one. When stdin
// is not a terminal (piped input / tests), it falls back to a buffered line
// read so scripted/test input still works.
func (p *stdinPrompter) Password(question string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", question)
	if p.file != nil && term.IsTerminal(int(p.file.Fd())) {
		b, err := term.ReadPassword(int(p.file.Fd()))
		fmt.Fprintln(p.out)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := p.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimSpace(line), nil
}
