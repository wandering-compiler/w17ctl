package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Machine is a plugin target of a swarm environment: deploy/<env>/machines/<name>/,
// whose machine.txt names its server (`ssh <user@host[:port]>`) and the images it runs
// (one `image <name>` line each). A plugin's services may run on a server of their own,
// beside the environment's swarm rather than in it; the deploy reaches that server over ssh.
type Machine struct {
	Name, Dir, SSH string
	Target         Target
	Images         []string
}

// ReadMachines reads envDir/machines/*/machine.txt, by name — none when the
// environment runs every plugin activation on its own server.
func ReadMachines(envDir string) ([]Machine, error) {
	paths, err := filepath.Glob(filepath.Join(envDir, "machines", "*", "machine.txt"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Machine
	for _, p := range paths {
		body, err := os.ReadFile(p) // #nosec G304 -- the project's own generated file
		if err != nil {
			return nil, err
		}
		m := Machine{Name: filepath.Base(filepath.Dir(p)), Dir: filepath.Dir(p)}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "ssh":
				m.SSH = val
			case "image":
				m.Images = append(m.Images, val)
			default:
				return nil, fmt.Errorf("%s: %q is not an `ssh` or `image` line — run `w17ctl codegen`", p, line)
			}
		}
		if m.SSH == "" {
			return nil, fmt.Errorf("%s names no server — run `w17ctl codegen`", p)
		}
		if m.Target, err = Parse(m.SSH); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, m)
	}
	return out, nil
}
