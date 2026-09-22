// Package gofmtc formats the Go the console generated, on the machine that
// asked for it.
//
// The console used to format. Measured on a 350-model project that was 33 s of
// CPU and 6.2 GB of allocation — 43% of the generate phase — spent on the one
// resource a codegen deployment is actually short of: the console runs a fixed
// number of concurrent jobs, while the client that asked for them sits idle
// waiting. Parallelising it there would have spent the same CPU faster on the
// contended box; spending it here spends none of it.
//
// Every generated Go file arrives with `// emit-sha256: <hex>` covering the
// body the generator produced. A file already on disk carrying the same marker
// IS the formatted form of those exact bytes, so it is left alone — not
// formatted, not even rewritten. For the ordinary edit-one-proto regen that is
// nearly every file in the project.
//
// Files nothing formats — the SPA's TypeScript, its JSON, the .po catalogues —
// need no marker to get the same treatment: what arrived is what belongs on
// disk, so comparing the bytes settles it. Before this they were rewritten on
// every run regardless, which is how a regen that changed one model still woke
// every file watcher in the project.
package gofmtc

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
)

// Marker matches the server's gofmtx.EmitMarker. Duplicated rather than
// imported: w17ctl is the PUBLIC client and must not pull in the compiler
// core (see the public-split architecture). A constant this small, pinned by
// a test on both sides, is the cheaper half of that trade.
const Marker = "// emit-sha256: "

// File is one generated file on its way to disk.
type File struct {
	Path     string
	Contents []byte
	// Skip is set when this project already holds the formatted form of
	// exactly these bytes, so there is nothing to write.
	Skip bool
}

// Result reports what a run did, so the caller can say so.
type Result struct {
	Formatted int
	Skipped   int
}

// Apply formats every Go file in the write set, in place, and marks the ones
// it could skip.
//
// It runs BEFORE the collision pre-scan on purpose. The scan compares what
// arrived against what is on disk; comparing unformatted against formatted
// would mark every file changed, which would both defeat the "nothing to do"
// path and destroy the author-file protection the scan exists for.
func Apply(root string, files []*File) (Result, error) {
	return ApplyMode(root, files, ModeEmbedded)
}

// ApplyMode is Apply with the formatter chosen explicitly.
//
// The pinned formatter is resolved ONCE per run, not per file: it may be a
// docker image, and paying that lookup 1500 times would dwarf the formatting.
func ApplyMode(root string, files []*File, mode Mode) (Result, error) {
	format := format.Source
	if mode == ModePinned {
		fn, using, err := pinnedFormatter(root)
		if err != nil {
			return Result{}, err
		}
		fmt.Fprintf(os.Stderr, "w17ctl: formatting generated Go with %s (--gofmt=pinned)\n", using)
		format = fn
	}
	return apply(root, files, format)
}

func apply(root string, files []*File, format func([]byte) ([]byte, error)) (Result, error) {
	var res Result
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			// Nothing formats these — the SPA's .ts/.tsx, its .json, the
			// .po catalogues — so what arrived IS what belongs on disk, and
			// a plain comparison answers the question the emit hash answers
			// for Go. No marker needed, which is just as well: .json has
			// nowhere to put a comment.
			if sameOnDisk(filepath.Join(root, f.Path), f.Contents) {
				f.Skip = true
				res.Skipped++
			}
			continue
		}
		incoming, ok := emitHash(f.Contents)
		if ok && onDiskHasHash(filepath.Join(root, f.Path), incoming) {
			f.Skip = true
			res.Skipped++
			continue
		}
		out, err := format(f.Contents)
		if err != nil {
			return res, fmt.Errorf("gofmt %s: the console emitted invalid Go: %w", f.Path, err)
		}
		f.Contents = out
		res.Formatted++
	}
	return res, nil
}

// emitHash reads the marker off the END of a file. It lives there so that
// `// Code generated … DO NOT EDIT.` keeps line one, where tools look for it.
func emitHash(body []byte) (string, bool) {
	tail := body
	if len(tail) > 256 {
		tail = tail[len(tail)-256:]
	}
	i := bytes.LastIndex(tail, []byte(Marker))
	if i < 0 {
		return "", false
	}
	h := strings.TrimSpace(string(tail[i+len(Marker):]))
	if len(h) != 64 {
		return "", false
	}
	return h, true
}

// SameOnDisk is sameOnDisk for callers outside this package — the embedded
// admin-runtime vendoring, which writes files nothing here ever sees but has
// exactly the same question to answer.
func SameOnDisk(path string, want []byte) bool { return sameOnDisk(path, want) }

// sameOnDisk reports whether the file already holds exactly these bytes.
//
// Size first: a changed file is usually a different length, and a stat is far
// cheaper than reading a file only to find the first byte differs.
//
// It does NOT compare the mode. A skipped file keeps whatever permissions the
// previous run gave it, which is the same thing that happens to a Go file
// skipped on its hash — worth knowing, not worth a read of every file to
// second-guess.
func sameOnDisk(path string, want []byte) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() != int64(len(want)) {
		return false
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Equal(got, want)
}

// onDiskHasHash reads only the last 256 bytes. A generated file is hundreds
// of lines; the answer is in the last one.
func onDiskHasHash(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	n := int64(256)
	if st.Size() < n {
		n = st.Size()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil {
		return false
	}
	got, ok := emitHash(buf)
	return ok && got == want
}

// SourceIfGo formats rel's contents when rel is a Go file and returns them
// untouched otherwise.
//
// For the paths that write what the console sent WITHOUT going through the
// codegen op stream: `target client add`, `guide`, `plugin gen-pb`. They have
// no emit marker to compare against and nothing to skip — they just have to
// format, because the console no longer does.
func SourceIfGo(rel string, contents []byte) ([]byte, error) {
	if !strings.HasSuffix(rel, ".go") {
		return contents, nil
	}
	out, err := format.Source(contents)
	if err != nil {
		return nil, fmt.Errorf("gofmt %s: the console emitted invalid Go: %w", rel, err)
	}
	return out, nil
}
