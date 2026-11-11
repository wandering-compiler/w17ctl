package prompter

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"golang.org/x/term"
)

// CodeReader is a Prompter that can read a code pasted from a browser: masked
// as it is typed, with a key that copies a link to the clipboard. The
// production Prompter is one; a test stub need not be.
type CodeReader interface {
	// ReadCode prints question and reads one code. At a terminal the input
	// is echoed as `*`, and `c` pressed on its own before anything else is
	// typed copies link to the clipboard (OSC 52). Anywhere else it is a
	// plain line read, as Text.
	ReadCode(ctx context.Context, question, link string) (string, error)
}

// ErrInputAborted is returned when the person cancels a masked read with
// Ctrl-C or Ctrl-D: in raw mode the terminal sends them as bytes, not as
// signals, so the read itself has to stop.
var ErrInputAborted = errors.New("cancelled")

// ReadCode reads a code masked when both stdin and stdout are terminals, and
// as a plain line otherwise (a pipe, CI, a test): those have no keys to press
// and no screen to hide the code from.
func (p *stdinPrompter) ReadCode(ctx context.Context, question, link string) (string, error) {
	if p.file == nil || p.outFile == nil || !term.IsTerminal(int(p.file.Fd())) || !term.IsTerminal(int(p.outFile.Fd())) {
		return p.Text(question, "")
	}
	fd := int(p.file.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return p.Text(question, "")
	}
	// The terminal goes back to normal however this ends: on return, and
	// when the caller gives up waiting (its context ends) while this read
	// is still blocked on a key that may never come.
	var once sync.Once
	restore := func() {
		once.Do(func() {
			fmt.Fprint(p.out, bracketedPasteOff)
			_ = term.Restore(fd, old)
		})
	}
	defer restore()
	stop := context.AfterFunc(ctx, restore)
	defer stop()
	// AfterFunc restores from a goroutine of its own, which a process on its
	// way out does not wait for: RestoreTerminal lets the caller restore
	// synchronously before it returns (a login that timed out).
	p.setRestore(restore)
	defer p.setRestore(nil)
	// A signal that ends the process (SIGTERM, SIGHUP — Ctrl-C is a byte in
	// raw mode, not a signal) would leave the terminal raw: restore it first,
	// then let the signal do what it would have done.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	defer func() { signal.Stop(sigs); close(done) }()
	go func() {
		select {
		case s := <-sigs:
			restore()
			signal.Stop(sigs)
			if pr, err := os.FindProcess(os.Getpid()); err == nil && pr.Signal(s) == nil {
				return
			}
			os.Exit(1)
		case <-done:
		}
	}()
	fmt.Fprint(p.out, bracketedPasteOn)
	return readMasked(p.file, p.out, question, link, os.Getenv("TMUX") != "")
}

// TerminalRestorer is a Prompter that may hold the terminal in raw mode while a
// read it started is still blocked (ReadCode after its context ended).
type TerminalRestorer interface {
	// RestoreTerminal puts the terminal back as it was, now; it does nothing
	// when no read holds it.
	RestoreTerminal()
}

func (p *stdinPrompter) setRestore(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restoreRaw = f
}

// RestoreTerminal restores a terminal a masked read left in raw mode.
func (p *stdinPrompter) RestoreTerminal() {
	p.mu.Lock()
	f := p.restoreRaw
	p.mu.Unlock()
	if f != nil {
		f()
	}
}

const (
	bracketedPasteOn  = "\x1b[?2004h"
	bracketedPasteOff = "\x1b[?2004l"
	pasteStart        = "\x1b[200~"
	pasteEnd          = "\x1b[201~"
)

// readMasked is the key loop of ReadCode, over a terminal already in raw
// mode: it reads keys from in and draws on out until Enter.
//
//   - Every character typed or pasted shows as one `*`.
//   - Backspace removes the last character, Ctrl-U the whole input.
//   - Enter submits; a line break inside a paste does not (a code copied with
//     its line end is still one code).
//   - Ctrl-C, and Ctrl-D on an empty input, cancel with ErrInputAborted.
//   - Other escape sequences (arrows, function keys) are ignored.
//   - `c` copies link — only when it is the whole of one read, outside a
//     paste, with nothing typed yet. A code is base64url and CAN start with
//     `c`, so the key cannot simply mean "copy" everywhere: a paste arrives
//     either inside the bracketed-paste markers or as a burst of many bytes in
//     one read, and a single keypress is the only thing that is neither. A
//     code typed by hand that starts with `c` has to start with the paste
//     instead, which nobody does for a 40-character code.
func readMasked(in io.Reader, out io.Writer, question, link string, tmux bool) (string, error) {
	prompt := question + ": "
	if link != "" {
		prompt = question + " (press c to copy the link): "
	}
	fmt.Fprint(out, prompt)

	var (
		code    []byte
		pending []byte // an escape sequence split across reads
		pasting bool
	)
	buf := make([]byte, 512)
	for {
		n, err := in.Read(buf)
		if n == 0 && err != nil {
			fmt.Fprint(out, "\r\n")
			if errors.Is(err, io.EOF) {
				return "", ErrInputAborted
			}
			return "", fmt.Errorf("read input: %w", err)
		}
		chunk := pending
		chunk = append(chunk, buf[:n]...)
		pending = nil

		if link != "" && !pasting && len(code) == 0 && string(chunk) == "c" {
			fmt.Fprint(out, osc52(link, tmux))
			fmt.Fprint(out, "\r\n  Link copied, if your terminal allows clipboard access; otherwise copy it from above.\r\n"+prompt)
			continue
		}

		for i := 0; i < len(chunk); i++ {
			b := chunk[i]
			if b == 0x1b {
				seq, ok := escapeSequence(chunk[i:])
				if !ok { // the rest of it is in the next read
					pending = append([]byte(nil), chunk[i:]...)
					break
				}
				switch seq {
				case pasteStart:
					pasting = true
				case pasteEnd:
					pasting = false
				}
				i += len(seq) - 1
				continue
			}
			switch {
			case b == '\r' || b == '\n':
				if pasting {
					continue
				}
				fmt.Fprint(out, "\r\n")
				return strings.TrimSpace(string(code)), nil
			case b == 0x03: // Ctrl-C
				fmt.Fprint(out, "\r\n")
				return "", ErrInputAborted
			case b == 0x04: // Ctrl-D
				if len(code) == 0 {
					fmt.Fprint(out, "\r\n")
					return "", ErrInputAborted
				}
			case b == 0x7f || b == 0x08: // Backspace
				if len(code) > 0 {
					_, size := utf8.DecodeLastRune(code)
					code = code[:len(code)-size]
					fmt.Fprint(out, "\b \b")
				}
			case b == 0x15: // Ctrl-U
				fmt.Fprint(out, strings.Repeat("\b \b", utf8.RuneCount(code)))
				code = code[:0]
			case b < 0x20:
				// another control key: nothing to draw, nothing to keep
			default:
				code = append(code, b)
				if !utf8.RuneStart(b) {
					continue // the rest of a character already drawn
				}
				fmt.Fprint(out, "*")
			}
		}
	}
}

// escapeSequence returns the escape sequence s starts with (s[0] is ESC), and
// false when s ends before the sequence does. CSI sequences (ESC [ … final)
// are read to their final byte; any other ESC takes the one byte after it.
func escapeSequence(s []byte) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	if s[1] == 0x1b {
		// Escape pressed on its own, then another sequence (a paste's
		// start marker): the lone ESC is the whole of this one, so the
		// marker after it is still read as a marker.
		return string(s[:1]), true
	}
	if s[1] != '[' {
		return string(s[:2]), true
	}
	for j := 2; j < len(s); j++ {
		if s[j] >= 0x40 && s[j] <= 0x7e {
			return string(s[:j+1]), true
		}
	}
	return "", false
}

// osc52 is the escape sequence that asks the terminal to put text on the
// system clipboard. It travels the terminal's own connection, so it works over
// ssh too. Inside tmux it is wrapped in tmux's passthrough (its ESCs doubled),
// which tmux hands to the terminal outside when `allow-passthrough` is on;
// tmux with `set-clipboard on` takes the bare one as well.
func osc52(text string, tmux bool) string {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	if !tmux {
		return seq
	}
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}
