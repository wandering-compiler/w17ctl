package infra

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// countdown is how long the confirmation waits before it accepts an answer.
const countdown = 10

// The seams the tests replace: the countdown's clock, the terminal check and
// the confirmation's input.
var (
	confirmSleep  = time.Sleep
	stdinTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
)

var confirmStdin io.Reader = os.Stdin

// typed is one name the operator types out: what it is, and its value.
type typed struct{ what, want string }

// confirm is the deliberate step before a command replaces data: text (what
// happens), a countdown during which no answer is taken, then each name
// typed out — not y. Refused, op's error ends in refused (what did not happen).
func confirm(op, text, refused string, names ...typed) error {
	fmt.Fprint(core.Stdout, text)
	tty := stdinTerminal()
	for i, n := range names {
		wait := func() { fmt.Fprintf(core.Stdout, "type %s (%s): ", n.what, n.want) }
		if i == 0 {
			wait = func() {
				for s := countdown; s > 0; s-- {
					fmt.Fprintf(core.Stdout, "\rtype %s in %2ds … ", n.what, s)
					confirmSleep(time.Second)
				}
				fmt.Fprintf(core.Stdout, "\rtype %s (%s): ", n.what, n.want)
			}
		}
		answer, err := readAnswer(confirmStdin, tty, wait)
		if err != nil {
			return fmt.Errorf("%s: reading the confirmation: %w — %s", op, err, refused)
		}
		if answer != n.want {
			return fmt.Errorf("%s: %q is not %q — %s", op, answer, n.want, refused)
		}
	}
	return nil
}

// readAnswer is the first line completed after wait (the countdown) returns.
// On a terminal a line entered during the countdown is read and dropped: only
// what is typed once asked counts. A pipe is read after the countdown as it
// is (its line was written on purpose). One byte at a time, and nothing after
// the answer: the stdin stays the operator's.
func readAnswer(in io.Reader, tty bool, wait func()) (string, error) {
	if !tty {
		wait()
		return readLine(in, func() bool { return true })
	}
	var asked atomic.Bool
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := readLine(in, asked.Load)
		got <- result{line, err}
	}()
	wait()
	asked.Store(true)
	r := <-got
	return r.line, r.err
}

// readLine reads lines from in until one completes while accept() holds.
func readLine(in io.Reader, accept func() bool) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := in.Read(b)
		if n == 1 {
			if b[0] != '\n' {
				line = append(line, b[0])
				continue
			}
			if accept() {
				return strings.TrimSpace(string(line)), nil
			}
			line = line[:0]
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 || !accept() {
				return "", errors.New("no answer (end of input)")
			}
			return strings.TrimSpace(string(line)), nil
		}
		if err != nil {
			return "", err
		}
	}
}
