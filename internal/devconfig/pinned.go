package devconfig

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PinnedPorts reads the host ports a project has CHOSEN, from the process
// environment and from the project's `.env`.
//
// It exists because the registry was answering a question the developer had
// already answered. `AllocatePorts` assigns a free port to any slot without
// one, and `BuildUpEnv` then passes that value to the compose subprocess —
// where it wins over `.env`, because a variable in the environment outranks
// the file. A developer who wrote
//
//	FINPLATFORM_GATEWAY_HOST_PORT=16720
//
// got 14003 and no explanation. On a host where each workspace has its own
// tunnelled block of ports, a number outside the block is not merely
// surprising: nothing outside it is reachable at all, so the stack comes up
// and cannot be used. A consumer reported exactly that.
//
// The registry keeps its real job — picking a port for a slot nobody has
// chosen, and not handing the same one to two projects. A pinned value is
// recorded in it for that second reason: an explicit choice has to be visible
// to the next project's allocator, or avoiding collisions stops working the
// moment anyone pins anything.
//
// Precedence within this function is the environment over `.env`, which is the
// order docker compose itself uses, so `W17_X=1 w17ctl stack up` behaves the
// way the same override behaves for a direct `docker compose up`.
func PinnedPorts(root string, slots []Slot) map[string]int {
	out := map[string]int{}
	fromFile := readDotEnv(filepath.Join(root, ".env"))
	for _, s := range slots {
		raw, ok := os.LookupEnv(s.Key)
		if !ok || strings.TrimSpace(raw) == "" {
			raw, ok = fromFile[s.Key]
			if !ok {
				continue
			}
		}
		// A value that is not a port is NOT a pin, and not an error here
		// either: compose will reject it with a better message than this
		// function could, naming the file and the service.
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n <= 0 || n > 65535 {
			continue
		}
		out[s.Key] = n
	}
	return out
}

// readDotEnv parses the subset of `.env` docker compose accepts for simple
// assignments: KEY=VALUE, one per line, `#` comments, optional quotes.
//
// Deliberately not a full dotenv implementation. Everything this reads is
// checked by Atoi above, so the worst an unsupported form (multi-line values,
// interpolation) can do is fail to look like a port and be skipped — which
// leaves the previous behaviour rather than producing a wrong one.
func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	// `_ =` on the deferred Close, the way the rest of w17ctl does it
	// (internal/docker/builder.go and friends): errcheck gates `make audit`, and a
	// read-only Close has nothing a caller could act on anyway.
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			out[k] = v
		}
	}
	return out
}
