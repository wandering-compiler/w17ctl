package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CoDevPath is W17_WANDERING_COMPILER_PATH — the wandering-compiler checkout a
// co-dev project builds against — as the PROJECT-ROOT-RELATIVE path every
// consumer of it joins onto the root (go.mod replace lines, go.work `use`s,
// the tool's own go.mod). Empty when it is not set.
//
// The variable may be written either way. Every reader used to take it as
// relative and only trimmed slashes, which turned an absolute
// `/home/me/w17` into `home/me/w17` — joined onto the root as a directory
// that does not exist, and rendered into the co-dev go.mods as
// `../../../home/me/w17`. An absolute path is now made relative to the root.
func CoDevPath(root string) (string, error) {
	p := strings.TrimSpace(os.Getenv("W17_WANDERING_COMPILER_PATH"))
	if p == "" {
		return "", nil
	}
	if !filepath.IsAbs(p) {
		return strings.Trim(filepath.ToSlash(filepath.Clean(p)), "/"), nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("W17_WANDERING_COMPILER_PATH: resolve the project root %s: %w", root, err)
	}
	rel, err := filepath.Rel(absRoot, filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("W17_WANDERING_COMPILER_PATH %s cannot be reached from the project root %s: %w", p, absRoot, err)
	}
	return filepath.ToSlash(rel), nil
}
