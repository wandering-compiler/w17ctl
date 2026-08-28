package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// ReadyProbe reports whether a local forward port is bound yet — i.e. the
// tunnel is carrying. It defaults to the shared OS bind probe (a bound
// port means ssh -L is holding it); tests stub it. WaitReady polls it.
var ReadyProbe = devconfig.PortInUse

// WaitReady polls until every forward's local port is bound (the tunnel is
// up) or the timeout elapses. Best-effort: it returns whether all ports
// came ready, but callers proceed regardless — a still-connecting forward
// surfaces as a normal DB-dial error rather than a hang.
func WaitReady(fwds []Forward, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		allReady := true
		for _, f := range fwds {
			if !ReadyProbe(f.LocalPort) {
				allReady = false
				break
			}
		}
		if allReady {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// State is the per-project runtime record of an open tunnel — written on
// `stack up` (remote) so a later `stack down` in a fresh w17ctl process
// can find and kill the detached ssh child. It lives under the w17ctl
// home (per-user runtime state), never the project tree or the lock.
type State struct {
	PID      int       `json:"pid"`
	Remote   string    `json:"remote"`   // the remote name (for `ps`/diagnostics)
	Forwards []Forward `json:"forwards"` // what was tunneled (diagnostics)
}

// StatePath returns the runtime state file for a project:
// <W17_HOME>/tunnels/<project>.json.
func StatePath(project string) (string, error) {
	dir, err := devconfig.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tunnels", project+".json"), nil
}

// SaveState writes the tunnel record for a project. The write is atomic
// (temp file in the same directory + rename): a reader — `stack down`,
// or withRemoteDB asking whether a tunnel is already open — must never
// observe an empty or half-written record, and a w17ctl killed mid-write
// must leave the previous record intact rather than a corrupt one that
// every later run fails to parse (T3-7 pass #9, C-F9).
func SaveState(project string, st State) error {
	path, err := StatePath(project)
	if err != nil {
		return err
	}
	return saveStateAt(path, st)
}

func saveStateAt(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return lockfile.WriteAtomic(path, data, 0o644)
}

// LoadState reads a project's tunnel record. A missing file returns
// (zero, false, nil) — no open tunnel, not an error.
func LoadState(project string) (State, bool, error) {
	path, err := StatePath(project)
	if err != nil {
		return State{}, false, err
	}
	return loadStateAt(path)
}

func loadStateAt(path string) (State, bool, error) {
	var st State
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, false, nil
		}
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, false, fmt.Errorf("tunnel: parse state %s: %w", path, err)
	}
	return st, true, nil
}

// RemoveState deletes a project's tunnel record (idempotent).
func RemoveState(project string) error {
	path, err := StatePath(project)
	if err != nil {
		return err
	}
	return removeStateAt(path)
}

func removeStateAt(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Replace swaps a project's tunnel: under the project's state lock it
// tears down whatever tunnel is recorded, calls open() to start the new
// ssh child, and records it. `stack up` goes through here.
//
// The lock is what makes the sequence a read-modify-WRITE rather than
// two blind writes. Without it two concurrent ups both read "nothing
// recorded", both spawn an ssh child, and the second record hides the
// first — an ssh process nothing can kill again, since `stack down` only
// knows the pid the file holds. Serialising is the correct shape here
// (not "one owner, second refused"): the second up re-reads INSIDE the
// section and tears the first tunnel down, which is exactly what a
// sequential pair of ups does.
func Replace(project string, open func() (State, error)) error {
	path, err := StatePath(project)
	if err != nil {
		return err
	}
	release, err := lockState(path)
	if err != nil {
		return err
	}
	defer release()
	if err := closeLocked(path); err != nil {
		return err
	}
	st, err := open()
	if err != nil {
		return err
	}
	return saveStateAt(path, st)
}

// Close tears down a project's recorded tunnel (kills the detached ssh
// child + drops the record) under the same lock. A no-op when none is
// open. `stack down` goes through here.
func Close(project string) error {
	path, err := StatePath(project)
	if err != nil {
		return err
	}
	release, err := lockState(path)
	if err != nil {
		return err
	}
	defer release()
	return closeLocked(path)
}

// closeLocked is the teardown body; the caller holds the state lock.
// Kept separate so Replace can tear down and re-record inside ONE
// critical section — taking the lock twice would self-deadlock, since
// distinct open descriptors contend even within one process.
func closeLocked(path string) error {
	st, ok, err := loadStateAt(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	_ = KillPID(st.PID)
	return removeStateAt(path)
}

// lockState takes the cross-process exclusive lock on a project's tunnel
// record. flock(2) via the shared lockfile helper: the kernel drops it on
// process death, so a killed `stack up` never wedges the next one.
func lockState(path string) (func(), error) {
	release, err := lockfile.ForUpdate(path)
	if err != nil {
		return nil, fmt.Errorf("tunnel: lock state %s: %w", path, err)
	}
	return release, nil
}

// KillPID signals the recorded forward child to stop. A pid ≤ 0 or an
// already-dead process is a no-op (not an error) — down should always
// converge to "no tunnel".
var KillPID = func(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil // unix: FindProcess never errors; guard for other OSes
	}
	if err := signalPID(p); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil // best-effort: a stale pid is fine, down still succeeds
	}
	return nil
}
