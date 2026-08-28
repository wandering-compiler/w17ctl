//go:build !unix

package lockfile

// ForUpdate is a best-effort no-op on platforms without flock(2). The
// concurrent-push lock-pin race (Q58-console-1) it guards on unix is
// unguarded here; w17ctl's real targets are unix (macOS dev + Docker
// linux), so this fallback keeps non-unix cross-builds compiling without
// claiming a guarantee it can't make.
func ForUpdate(_ string) (func(), error) {
	return func() {}, nil
}
