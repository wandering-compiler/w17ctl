package devconfig

import (
	"sort"
	"strconv"
)

// BuildUpEnv merges a project's managed host-port assignments with a
// preset's extra env into a sorted KEY=VALUE list for the compose
// subprocess. Preset env wins on conflict (it is layered last).
//
// This is the single source of truth for "what env does `stack up` /
// the UI inject" — both the w17ctl CLI and the local UI call it, so a
// human clicking the UI and an agent driving the CLI produce identical
// compose invocations.
func BuildUpEnv(p *Project, preset *Preset) []string {
	merged := make(map[string]string, len(p.Ports))
	for k, v := range p.Ports {
		merged[k] = strconv.Itoa(v)
	}
	if preset != nil {
		for k, v := range preset.Env {
			merged[k] = v
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(merged))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// PresetServices picks the services to bring up: an explicit list wins;
// otherwise the preset's selection; otherwise nil (the whole stack).
func PresetServices(explicit []string, preset *Preset) []string {
	if len(explicit) > 0 {
		return explicit
	}
	if preset != nil {
		return preset.Services
	}
	return nil
}
