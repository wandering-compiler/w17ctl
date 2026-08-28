package consoleui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// dockerCmdFn is the indirection point for tests — it runs a docker
// subprocess and returns its stdout. Production shells out to the real
// `docker` binary; tests stub it to return canned JSON.
var dockerCmdFn = realDockerCmd

func realDockerCmd(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// Surface docker's own stderr — "Cannot connect to the Docker
		// daemon" is the common case and the user needs to see it.
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return nil, fmt.Errorf("docker %s: %s", strings.Join(args, " "), msg)
		}
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// Container is one row of `docker compose ps` / `docker stats`, flattened
// to the fields the UI shows. Names mirror docker's JSON keys so the
// frontend can rely on them.
type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"`  // running / exited / …
	Status  string `json:"status"` // "Up 3 minutes", "Exited (0) …"
	Ports   string `json:"ports"`

	// Live resource use, filled from `docker stats` (empty when the
	// container is not running or stats are unavailable).
	CPUPerc  string  `json:"cpuPerc,omitempty"`
	MemUsage *string `json:"memUsage,omitempty"`
	MemPerc  string  `json:"memPerc,omitempty"`
}

// composePS lists a project's containers via `docker compose ps`. The
// project dir holds compose.yaml + compose.w17.yaml, so compose resolves
// the right project. Output is NDJSON (one object per line) on modern
// compose; we also tolerate a single JSON array.
func composePS(ctx context.Context, dir string) ([]Container, error) {
	raw, err := dockerCmdFn(ctx, dir, "compose", "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	rows, err := decodeComposePS(raw)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// composePSRow is the subset of `docker compose ps --format json` we read.
type composePSRow struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	Image   string `json:"Image"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Ports   string `json:"Ports"`
}

func decodeComposePS(raw []byte) ([]Container, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []Container{}, nil
	}
	var rows []composePSRow
	if trimmed[0] == '[' {
		// Older compose: a single JSON array.
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("ui: parse compose ps array: %w", err)
		}
	} else {
		// Modern compose: NDJSON, one object per line.
		for _, line := range bytes.Split(trimmed, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var r composePSRow
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("ui: parse compose ps line: %w", err)
			}
			rows = append(rows, r)
		}
	}
	out := make([]Container, 0, len(rows))
	for _, r := range rows {
		out = append(out, Container{
			ID:      r.ID,
			Name:    r.Name,
			Service: r.Service,
			Image:   r.Image,
			State:   r.State,
			Status:  r.Status,
			Ports:   r.Ports,
		})
	}
	return out, nil
}

// statsRow is the subset of `docker stats --format "{{json .}}"` we read.
type statsRow struct {
	ID       string `json:"ID"`
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
}

// dockerStats returns a one-shot resource snapshot of all running
// containers, keyed by container ID (short or long — callers match by
// prefix). Empty map when nothing runs.
func dockerStats(ctx context.Context) (map[string]statsRow, error) {
	raw, err := dockerCmdFn(ctx, "", "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	byID := map[string]statsRow{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var r statsRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("ui: parse docker stats line: %w", err)
		}
		byID[r.ID] = r
	}
	return byID, nil
}

// mergeStats fills live CPU/mem fields into containers by matching the
// stats snapshot on container-ID prefix (compose ps gives the long ID,
// stats the short one).
func mergeStats(containers []Container, stats map[string]statsRow) {
	for i := range containers {
		for id, s := range stats {
			if id != "" && strings.HasPrefix(containers[i].ID, id) {
				containers[i].CPUPerc = s.CPUPerc
				m := s.MemUsage
				containers[i].MemUsage = &m
				containers[i].MemPerc = s.MemPerc
				break
			}
		}
	}
}
