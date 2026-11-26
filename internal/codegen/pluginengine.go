package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// codegenMinor is the codegen minor a codegen_version pin generates with: an
// empty pin is v0.1, for good; a release candidate is its minor's.
func codegenMinor(pin string) string {
	if pin == "" {
		return "v0.1"
	}
	v := pin
	if semver.MajorMinor(v) == v || !semver.IsValid(v) {
		v += ".0"
	}
	return semver.MajorMinor(v)
}

// checkPluginEngines refuses a run whose project installs a plugin written for
// a newer codegen minor than the one the project generates with. A plugin's
// requires_sdk names the SDK it needs, and an SDK minor is generated only by
// the engine of the same minor: an older engine cannot read what the plugin
// relies on and fails somewhere far from the cause — it dropped the plugin's
// preset, and told the project its include names a plugin that ships none.
func checkPluginEngines(root, protoDir, codegenVersion string) error {
	have := codegenMinor(codegenVersion)
	if !semver.IsValid(have) {
		return nil // an invalid pin is the console's refusal to make
	}
	manifests, _ := filepath.Glob(filepath.Join(root, protoDir, "plugins", "*", "plugin.yaml"))
	sort.Strings(manifests)
	for _, path := range manifests {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m struct {
			Name        string `yaml:"name"`
			Version     string `yaml:"version"`
			RequiresSdk string `yaml:"requires_sdk"`
		}
		if yaml.Unmarshal(data, &m) != nil || !semver.IsValid(m.RequiresSdk) {
			continue
		}
		need := semver.MajorMinor(m.RequiresSdk)
		if semver.Compare(need, have) <= 0 {
			continue
		}
		pin := fmt.Sprintf("codegen_version %q", codegenVersion)
		if codegenVersion == "" {
			pin = "no codegen_version, which means v0.1"
		}
		return fmt.Errorf("codegen: plugin %s %s needs SDK %s, which only the %s codegen engine generates — this project generates with %s (w17/lock.yaml pins %s)\n"+
			"  fix: move the project with `w17ctl codegen-version %s`, then `w17ctl sdk update`; or keep a version of %s written for %s",
			m.Name, m.Version, m.RequiresSdk, need, have, pin, need, m.Name, have)
	}
	return nil
}
