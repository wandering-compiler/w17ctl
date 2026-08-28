package devconfig

import "sort"

// AllocatePorts ensures every slot in `slots` has a unique host port
// assigned to the named project, allocating new ones from the config's
// port base upward while never colliding with a port already handed to
// ANY registered project. It is idempotent: an existing slot keeps its
// port across runs (so a project's gateway stays on the same host port
// even as other slots are added), and slots no longer present are
// pruned. Returns true when the project's Ports map changed.
//
// Beyond avoiding cross-project assignments in this config, it also
// skips any port currently bound on the OS (via [portInUse]) so a fresh
// slot never lands on a port a hand-run service (e.g. the dockerized
// console) happens to hold right now. The probe is best-effort and only
// consulted when picking a NEW port — existing sticky assignments are
// never disturbed.
func (c *Config) AllocatePorts(name string, slots []Slot) bool {
	p := c.Projects[name]
	if p == nil {
		return false
	}
	if p.Ports == nil {
		p.Ports = map[string]int{}
	}

	// Desired slot keys (deduped).
	want := make(map[string]bool, len(slots))
	for _, s := range slots {
		want[s.Key] = true
	}

	changed := false

	// Prune slots that no longer exist in the project.
	for k := range p.Ports {
		if !want[k] {
			delete(p.Ports, k)
			changed = true
		}
	}

	// Ports already taken: every other project's full set, plus this
	// project's surviving assignments (which we keep as-is).
	used := map[int]bool{}
	for n, pr := range c.Projects {
		if n == name {
			continue
		}
		for _, port := range pr.Ports {
			used[port] = true
		}
	}
	for _, port := range p.Ports {
		used[port] = true
	}

	// Assign any missing slot a fresh port, in a stable order so the
	// mapping is deterministic across runs.
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	next := c.EffectivePortBase()
	for _, k := range keys {
		if _, ok := p.Ports[k]; ok {
			continue
		}
		for used[next] || portInUse(next) {
			next++
		}
		p.Ports[k] = next
		used[next] = true
		next++
		changed = true
	}
	return changed
}
