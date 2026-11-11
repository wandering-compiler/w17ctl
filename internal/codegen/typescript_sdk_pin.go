package codegen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wandering-compiler/w17ctl/internal/tssdk"
)

// repinTypeScriptBusinessSDK is the TypeScript half of `codegen
// --sdk-version` (W17_SDK_VERSION), the twin of repinPythonBusinessSDK: it
// writes every generated TypeScript business image
// (`<servicesDir>/<domain>-business/Dockerfile`) against the override — the
// console rendered them at the lock's sdk_version. A release builds from its
// tag `ts/<version>`, a pseudo-version (an SDK under review) from its commit
// (tssdk.Ref); the one line rewritten is the image's
// `ARG W17_SDK_TS_REF=<ref>`. A co-dev image builds the checkout's
// sdk/ts/business and carries no ref to re-pin; a Go or Python image carries
// none either.
func repinTypeScriptBusinessSDK(root, servicesDir, version string, stdout io.Writer) error {
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
		out, ok := tssdk.Repin(body, version)
		if !ok || string(out) == string(body) {
			continue
		}
		if err := os.WriteFile(p, out, 0o644); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		fmt.Fprintf(stdout, "codegen: pinned the TypeScript business SDK at %s in %d image(s) (from --sdk-version; the lock keeps its own)\n", tssdk.Ref(version), n)
	}
	return nil
}
