package codegen

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/rssdk"
)

// repinRustBusinessSDK is the Rust half of `codegen --sdk-version`
// (W17_SDK_VERSION), the twin of repinTypeScriptBusinessSDK: it writes every
// generated Rust business image (`<servicesDir>/<domain>-business/Dockerfile`)
// against the override — the console rendered them at the lock's
// sdk_version. A release takes its tag `rs/<version>`, a pseudo-version (an
// SDK under review) its commit (rssdk.Ref); the one line rewritten is the
// image's `ARG W17_SDK_RS_REF=<ref>`, and the image decides from it at build
// time whether it builds the crate's Cargo.lock or serves that commit. A
// co-dev image builds the checkout's rs/w17-business and carries no ref to
// re-pin; a Go, Python or TypeScript image carries none either.
func repinRustBusinessSDK(root, servicesDir, version string, stdout io.Writer) error {
	if version == "" || servicesDir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(servicesDir), "*-business", "Dockerfile"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	n := 0
	for _, p := range matches {
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out, ok := rssdk.Repin(body, version)
		if !ok || string(out) == string(body) {
			continue
		}
		if err := os.WriteFile(p, out, 0o644); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		fmt.Fprintf(stdout, "codegen: pinned the Rust business SDK at %s in %d image(s) (from --sdk-version; the lock keeps its own)\n", rssdk.Ref(version), n)
	}
	return nil
}

// rustManifest is what warnRustCargoLocks reads of a generated
// w17-business.json: the language, and where the author's crate is.
type rustManifest struct {
	Language string `json:"language"`
	Register struct {
		Path string `json:"path"`
	} `json:"register"`
}

// warnRustCargoLocks compares, for every generated Rust business tier, the
// author's Cargo.lock with the tag its image builds the SDK at — the check
// the release image makes before `cargo build --locked`
// (rssdk.CheckCargoLock), made here on the developer's disk so a mismatch is
// seen at codegen, not at the end of an image build. It warns and never
// edits the crate: the author's Cargo.toml names the SDK, and only the
// author changes it. An image without a tag — co-development, or a
// pseudo-version (`--sdk-version`) — serves an SDK that is not a release and
// re-resolves the lock itself, so it is not judged. Run after the re-pins,
// so it judges the ref the images now carry.
func warnRustCargoLocks(root, servicesDir string, stdout io.Writer) error {
	if servicesDir == "" {
		return nil
	}
	manifests, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(servicesDir), "*-business", "w17-business.json"))
	if err != nil {
		return err
	}
	sort.Strings(manifests)
	for _, mp := range manifests {
		raw, err := os.ReadFile(mp)
		if err != nil {
			return err
		}
		var m rustManifest
		if json.Unmarshal(raw, &m) != nil || m.Language != "rust" || m.Register.Path == "" {
			continue
		}
		bundle := filepath.Base(filepath.Dir(mp))
		docker, err := os.ReadFile(filepath.Join(filepath.Dir(mp), "Dockerfile"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		ref, ok := rssdk.RefOf(docker)
		if !ok || !rssdk.IsTag(ref) {
			continue
		}
		crate, err := containedJoin(root, m.Register.Path)
		if err != nil {
			fmt.Fprintf(stdout, "codegen: warning: %s: the crate directory %q is not inside the project (%v)\n", bundle, m.Register.Path, err)
			continue
		}
		lockRel := filepath.ToSlash(filepath.Join(m.Register.Path, "Cargo.lock"))
		lock, err := os.ReadFile(filepath.Join(crate, "Cargo.lock"))
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stdout, "codegen: warning: %s: %s is missing — the image builds the crate from it (--locked) and refuses without it\n"+
				"  fix: run `cargo generate-lockfile` in %s and commit Cargo.lock\n", bundle, lockRel, m.Register.Path)
			continue
		}
		if err != nil {
			return err
		}
		if err := rssdk.CheckCargoLock(lock, ref); err != nil {
			fmt.Fprintf(stdout, "codegen: warning: %s: %s — the image build will refuse it\n  %s\n", bundle, lockRel, strings.ReplaceAll(err.Error(), "\n", "\n  "))
		}
	}
	return nil
}
