package codegen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wandering-compiler/w17ctl/internal/pysdk"
)

// repinPythonBusinessSDK is the Python half of `codegen --sdk-version`
// (W17_SDK_VERSION): resolveSdkGoPinsAt writes the generated go.mods against
// the override, and this writes every generated Python business image
// (`<servicesDir>/<domain>-business/Dockerfile`) against it too — the console
// rendered them at the lock's sdk_version, which the override does not
// change. A release installs from its tag `py/<version>`, a pseudo-version
// (an SDK under review) from its commit (pysdk.Ref). A co-dev image installs
// the checkout's sdk/py and carries no requirement to re-pin.
func repinPythonBusinessSDK(root, servicesDir, version string, stdout io.Writer) error {
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
		out, ok := pysdk.Repin(body, version)
		if !ok || string(out) == string(body) {
			continue
		}
		if err := os.WriteFile(p, out, 0o644); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		fmt.Fprintf(stdout, "codegen: pinned the Python business SDK at %s in %d image(s) (from --sdk-version; the lock keeps its own)\n", pysdk.Ref(version), n)
	}
	return nil
}
