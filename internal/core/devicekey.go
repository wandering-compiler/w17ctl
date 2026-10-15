package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// The device key: the private key this machine signs DPoP proofs (RFC 9449)
// with. A console whose auth binds devices binds every token it mints to the
// key that signed the sign-in's proof, and from then on accepts the token only
// together with a fresh proof signed by that key — a token copied off this
// machine is useless without the key file.
//
// ONE key per console origin: `<W17_HOME or ~/.w17>/device-<hash>.key`, the
// hash taken over the origin of the address w17ctl dials (dialOrigin). Per console,
// not per machine, because a console can revoke a key and w17ctl then deletes
// it: with one shared key, a revocation by ANY console — a dev stack, another
// organization's self-hosted one, a hostile one — would take every other
// console's session with it. The key is made by the first `login` to that
// console, kept 0600, and refused when anyone else can read it.

// EnvDPoPKeyVar names a key file to use instead of the per-console ones in the
// w17ctl home. It is the unattended half of the device key: a CI job sets it
// beside W17_TOKEN, pointing at the key the machine account was created with.
// With W17_TOKEN, it is also what turns signing on — a token from the
// environment is sent with a proof only when this is set.
const EnvDPoPKeyVar = "W17_DPOP_KEY"

// EnvDPoPOriginVar overrides the origin proofs are signed for. The console
// compares each proof's htu with the public origin it is configured with
// (its rpc surface's DPoP origin); w17ctl derives that origin from the
// address it dials (DPoPOrigin), and this is for a console reached by a name
// its configuration does not use. It changes only the signed origin: the key
// file is chosen by the dialed one.
const EnvDPoPOriginVar = "W17_CONSOLE_DPOP_ORIGIN"

// DeviceKeyPath returns where the device key for the console at addr lives,
// and whether the path came from W17_DPOP_KEY (a key the operator manages,
// which w17ctl never deletes).
func DeviceKeyPath(addr string) (path string, explicit bool, err error) {
	if p := os.Getenv(EnvDPoPKeyVar); p != "" {
		return p, true, nil
	}
	dir, err := devconfig.DefaultDir()
	if err != nil {
		return "", false, err
	}
	// The DIAL origin, never W17_CONSOLE_DPOP_ORIGIN: the override changes what
	// proofs are signed for, and must not silently switch to another key file
	// (and so to no session).
	sum := sha256.Sum256([]byte(dialOrigin(addr)))
	return filepath.Join(dir, "device-"+hex.EncodeToString(sum[:8])+".key"), false, nil
}

// deviceKeys caches the keys this process read, by file.
var (
	deviceKeyMu sync.Mutex
	deviceKeys  = map[string]*dpop.Key{}
)

// errNoDeviceKey is a token bound to a key this machine no longer has.
var errNoDeviceKey = errors.New("no device key")

// LoadDeviceKey returns the device key for the console at addr. A missing
// file is errNoDeviceKey (wrapped): it is never created here, because a token
// is bound to the key it was minted with and a new key cannot sign for it.
func LoadDeviceKey(addr string) (*dpop.Key, error) {
	path, _, err := DeviceKeyPath(addr)
	if err != nil {
		return nil, err
	}
	deviceKeyMu.Lock()
	defer deviceKeyMu.Unlock()
	return loadDeviceKeyLocked(path)
}

// reloadDeviceKey drops the cached key for the console and reads the file
// again.
func reloadDeviceKey(addr string) (*dpop.Key, error) {
	path, _, err := DeviceKeyPath(addr)
	if err != nil {
		return nil, err
	}
	deviceKeyMu.Lock()
	defer deviceKeyMu.Unlock()
	delete(deviceKeys, path)
	return loadDeviceKeyLocked(path)
}

func loadDeviceKeyLocked(path string) (*dpop.Key, error) {
	if k := deviceKeys[path]; k != nil {
		return k, nil
	}
	k, err := readDeviceKey(path)
	if err != nil {
		return nil, err
	}
	deviceKeys[path] = k
	return k, nil
}

// readDeviceKey reads a key file, refusing one other users could misuse: it
// holds the private key every bound session rests on (the rule ssh applies to
// its own keys).
//
// A key w17ctl keeps in its home must be private to the user (no group or
// other bits). A key the operator names with W17_DPOP_KEY is held only to not
// being writable by group or others: it is typically a mounted secret, and the
// common mounts are readable beyond the owner — a Kubernetes Secret volume
// (0644 by default, group-readable again under fsGroup) or a swarm secret
// under /run/secrets (0444) — inside a container that runs one user anyway.
// A key others can REPLACE binds nothing, wherever it is.
func readDeviceKey(path string) (*dpop.Key, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s", errNoDeviceKey, path)
		}
		return nil, fmt.Errorf("device key %s: %w", path, err)
	}
	mask := fs.FileMode(0o077)
	if os.Getenv(EnvDPoPKeyVar) == path {
		mask = 0o022
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&mask != 0 {
		return nil, fmt.Errorf("device key %s is accessible to other users (mode %04o) and is not used\n"+
			"  fix: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	k, err := dpop.LoadKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: device key %s: %w\n"+
			"  the file is damaged; delete it and sign in again (w17ctl login makes a new key)", errBadDeviceKey, path, err)
	}
	return k, nil
}

// errBadDeviceKey is a key file that exists and does not parse.
var errBadDeviceKey = errors.New("unreadable device key")

// EnsureDeviceKey returns the device key for the console at addr, generating
// and saving a new one (P-256, mode 0600) when the file does not exist. Called
// where a NEW binding is made — sign-in — and by `w17ctl key public`; every
// other call only loads.
func EnsureDeviceKey(addr string) (*dpop.Key, error) {
	path, _, err := DeviceKeyPath(addr)
	if err != nil {
		return nil, err
	}
	deviceKeyMu.Lock()
	defer deviceKeyMu.Unlock()
	k, err := loadDeviceKeyLocked(path)
	if err == nil || !errors.Is(err, errNoDeviceKey) {
		return k, err
	}
	if k, err = createDeviceKeyFile(path); err != nil {
		return nil, err
	}
	deviceKeys[path] = k
	return k, nil
}

// createDeviceKeyFile makes a key at path unless one is already there, and
// returns whichever key the file holds afterwards.
//
// Two processes can do this at once (two shells logging in), so the file is
// never renamed over: the new key is written whole to a temporary file and
// LINKED into place, which fails when path exists. The loser of the race reads
// the winner's key instead of replacing it — a replaced key would orphan the
// session the winner just bound to it. A file system without hard links gets
// the same rule from an exclusive create.
func createDeviceKeyFile(path string) (*dpop.Key, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".device-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	tmp := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(tmp) }()
	if err := k.SaveFile(tmp); err != nil {
		return nil, err
	}
	err = linkFn(tmp, path)
	switch {
	case err == nil:
		return k, nil
	case errors.Is(err, fs.ErrExist):
		return readRacedDeviceKey(path)
	}
	// No hard links here (EPERM, ENOTSUP, EXDEV, …): an exclusive create.
	return createDeviceKeyExclusive(path, k)
}

// linkFn is os.Link; tests swap it for a file system without hard links.
var linkFn = os.Link

func createDeviceKeyExclusive(path string, k *dpop.Key) (*dpop.Key, error) {
	data, err := k.MarshalPEM()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readRacedDeviceKey(path)
		}
		return nil, fmt.Errorf("device key: %w", err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("device key: %w", err)
	}
	return k, nil
}

// readRacedDeviceKey reads the key another process just created. With an
// exclusive create the winner may still be writing it, so a key that does not
// parse yet is read again a few times before it counts as damaged.
func readRacedDeviceKey(path string) (*dpop.Key, error) {
	var err error
	for range 10 {
		var k *dpop.Key
		if k, err = readDeviceKey(path); err == nil || !errors.Is(err, errBadDeviceKey) {
			return k, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

// discardRevokedDeviceKey deletes the console's device key after it answered
// DEVICE_REVOKED to a proof signed with the key whose thumbprint is given. A
// revoked device is refused for good, so the key has no further use; the next
// `login` makes a new one and becomes a new device.
//
// It removes the file only while it still holds that key: another shell may
// already have signed in again with a new key, and that key must survive a
// refusal meant for the old one. A key named by W17_DPOP_KEY belongs to the
// operator and is left in place (the refusal's explanation says to replace
// it). Reports whether the file was removed.
func discardRevokedDeviceKey(addr, thumbprint string) bool {
	path, explicit, err := DeviceKeyPath(addr)
	if err != nil {
		return false
	}
	deviceKeyMu.Lock()
	defer deviceKeyMu.Unlock()
	if k := deviceKeys[path]; k != nil && k.Thumbprint() == thumbprint {
		delete(deviceKeys, path)
	}
	if explicit {
		return false
	}
	onDisk, err := dpop.LoadKeyFile(path)
	if err != nil || onDisk.Thumbprint() != thumbprint {
		return false
	}
	return os.Remove(path) == nil
}

// resetDeviceKeyCache forgets the cached keys and nonces — a fresh process
// (tests).
func resetDeviceKeyCache() {
	deviceKeyMu.Lock()
	deviceKeys = map[string]*dpop.Key{}
	deviceKeyMu.Unlock()
	resetDPoPCreds()
}

// DPoPOrigin is the origin w17ctl signs proofs for when it dials addr: the
// proof's htu is this plus the gRPC method ("/pkg.Service/Method"), and the
// console compares it with its rpc surface's configured DPoP origin.
//
// w17ctl always dials a console over TLS, so the origin is https://host[:port]
// with the host lowercased and the default port 443 left out — the form a
// console's configuration names itself by (`https://api.example.com:50051`).
// A readability scheme (grpcs://) and a gRPC resolver prefix (dns:///) are
// dropped. W17_CONSOLE_DPOP_ORIGIN overrides the derivation.
func DPoPOrigin(addr string) string {
	if o := strings.TrimSpace(os.Getenv(EnvDPoPOriginVar)); o != "" {
		return strings.TrimSuffix(o, "/")
	}
	return dialOrigin(addr)
}

// dialOrigin is DPoPOrigin without the override: the console as dialed.
func dialOrigin(addr string) string {
	_, target := splitConsoleScheme(addr)
	if i := strings.Index(target, ":///"); i >= 0 {
		target = target[i+len(":///"):]
	} else if i := strings.Index(target, "://"); i >= 0 {
		// dns://authority/host:port — the authority is the resolver's, not
		// the console's.
		rest := target[i+len("://"):]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
		}
		target = rest
	}
	target = strings.TrimSuffix(target, "/")
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = strings.TrimSuffix(strings.TrimPrefix(target, "["), "]"), ""
	}
	host = strings.ToLower(host)
	if port == "" || port == "443" {
		if strings.Contains(host, ":") {
			return "https://[" + host + "]"
		}
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}
