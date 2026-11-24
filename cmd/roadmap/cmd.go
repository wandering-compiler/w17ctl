// Package roadmap is `w17ctl roadmap`: w17's public roadmap — the features
// each release is to bring, as w17's operator keeps it on the console.
package roadmap

import (
	"fmt"
	"io"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	supportpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/support"
)

// Cmd is `w17ctl roadmap [--release X]`.
type Cmd struct {
	Release string `name:"release" placeholder:"VERSION" help:"Only this release (0.2.0). Default: every release on the roadmap."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console endpoint. Defaults to the logged-in console."`
}

func (c *Cmd) Run() error {
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return fmt.Errorf("roadmap: %w", err)
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.Roadmap.ReadRoadmap(ctx, &supportpb.ReadRoadmapReq{Release: c.Release})
	if err != nil {
		return fmt.Errorf("roadmap: %w", err)
	}
	print(core.Stdout, resp.GetItems(), c.Release)
	return nil
}

var statusMark = map[supportpb.RoadmapItem_Status]string{
	supportpb.RoadmapItem_PLANNED:     "planned",
	supportpb.RoadmapItem_IN_PROGRESS: "in progress",
	supportpb.RoadmapItem_SHIPPED:     "shipped",
}

// print lays the items out release by release, in the console's order.
func print(w io.Writer, items []*supportpb.PublicRoadmapItem, release string) {
	if len(items) == 0 {
		if release != "" {
			fmt.Fprintf(w, "nothing on the roadmap for %s\n", release)
		} else {
			fmt.Fprintln(w, "the roadmap is empty")
		}
		return
	}
	cur := ""
	for _, it := range items {
		if it.GetRelease() != cur {
			if cur != "" {
				fmt.Fprintln(w)
			}
			cur = it.GetRelease()
			fmt.Fprintf(w, "%s\n", cur)
		}
		mark := statusMark[it.GetStatus()]
		if mark == "" {
			mark = "planned"
		}
		fmt.Fprintf(w, "  [%s] %s\n", mark, it.GetTitle())
		if s := strings.TrimSpace(it.GetSummary()); s != "" {
			for _, l := range strings.Split(s, "\n") {
				fmt.Fprintf(w, "      %s\n", l)
			}
		}
	}
}
