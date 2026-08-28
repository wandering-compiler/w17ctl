package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DetectAclSurface reports whether any domain proto under
// `<root>/<protoDir>/domains/` carries an ACL config option (the
// `w17.acl_` textual prefix). A hit means codegen should refresh the
// per-domain `w17.lock.acl.proto`, and verify should check it for drift.
func DetectAclSurface(root, protoDir string) bool {
	if protoDir == "" {
		protoDir = "proto"
	}
	domainsRoot := filepath.Join(root, protoDir, "domains")
	found := false
	_ = filepath.WalkDir(domainsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), "w17.acl_") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// HasAclLockFile reports whether a committed `w17.lock.acl.proto` exists under
// `<root>/<protoDir>/domains/`.
//
// This is the trigger for verifying the ACL lock, and DetectAclSurface is not.
// The detector answers "does the project still declare an ACL surface", which
// is a question the gate must ASK the server, never the condition for asking
// it: a lock that outlived its surface is precisely the drift the ACL verifier
// reports, and it is unreachable if a missing surface skips the check. The
// detector is also a content grep, and a generated lock names `w17.acl_` in a
// header comment that no checksum or signature covers — so the file being
// verified could switch off its own verification (T2-5 pass #9, D-F1).
//
// Filename lookup, not content: a name is what the emitter guarantees, and it
// cannot be edited without the file ceasing to be the lock.
func HasAclLockFile(root, protoDir string) bool {
	return hasLockFileNamed(root, protoDir, "w17.lock.acl.proto")
}

// hasLockFileNamed is the shared walk behind both lock triggers. One
// implementation because the two arms drifted apart once already: the ACL one
// was hardened to trigger on the lock file and the eventbus one was not.
func hasLockFileNamed(root, protoDir, name string) bool {
	if protoDir == "" {
		protoDir = "proto"
	}
	found := false
	_ = filepath.WalkDir(filepath.Join(root, protoDir, "domains"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) == name {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// HasEventbusLockFile reports whether a committed `w17.lock.events.proto`
// exists under `<root>/<protoDir>/domains/`.
//
// Sibling of HasAclLockFile, and it exists for the same reason (T2-5 pass #13,
// D13-4). The ACL arm was hardened to trigger on the LOCK as well as on the
// surface, precisely because a lock that outlived its surface is the drift the
// verifier reports and is unreachable if a missing surface skips the check.
// The eventbus arm triggered on surface detection alone, so an orphaned
// eventbus lock was silently never verified — the same defect the ACL side had
// already been fixed for, on the sibling nobody revisited.
func HasEventbusLockFile(root, protoDir string) bool {
	return hasLockFileNamed(root, protoDir, "w17.lock.events.proto")
}

// DetectEventbusSurface reports whether the project declares an eventbus
// surface — an `events.proto` / `subscribers.proto` sentinel, or a
// plugin-contributed events tree behind a domain plugin activation.
func DetectEventbusSurface(root, protoDir string) bool {
	if protoDir == "" {
		protoDir = "proto"
	}
	domainsRoot := filepath.Join(root, protoDir, "domains")
	found := false
	_ = filepath.WalkDir(domainsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == "events.proto" || base == "subscribers.proto" || strings.HasPrefix(base, "subscribers_") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if found {
		return true
	}
	// Plugin-contributed events: an activated plugin can ship its own
	// events; the storage emit interceptor references the domain
	// envelope for those, so the envelope must be generated even when the
	// domain authors no events.proto of its own.
	return DomainsActivatePlugin(domainsRoot) &&
		pluginTreeHasEvents(filepath.Join(root, protoDir, "plugins"))
}

// DomainsActivatePlugin reports whether any domain sentinel declares a
// `(w17.domain).plugins[]` activation, detected by the `source_name:`
// key every activation entry carries.
func DomainsActivatePlugin(domainsRoot string) bool {
	found := false
	_ = filepath.WalkDir(domainsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) != "w17.proto" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), "source_name") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// pluginTreeHasEvents reports whether any installed plugin under
// `<protoDir>/plugins/` ships an events proto (`events.proto` or
// `*_events.proto`).
func pluginTreeHasEvents(pluginsRoot string) bool {
	found := false
	_ = filepath.WalkDir(pluginsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == "events.proto" || strings.HasSuffix(base, "_events.proto") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}
