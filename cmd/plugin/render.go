package plugin

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/pluginrender"
)

// RenderCmd turns an author tree into the PUBLISHED form a registry serves.
//
// The step between writing a plugin and releasing it, and the one an external
// author had no way to run: it lived as a shell loop in this repository's
// Makefile, which somebody outside the team does not have. `sign` and `gen-pb`
// were already here; this is the piece that was missing between them.
//
// ⚠️ It renders, and nothing else. No git, no remote, no tag — so it is safe to
// run over and over while writing a plugin, and it is what `plugin dev` uses to
// install the tree the way a CONSUMER will receive it rather than the way the
// author happens to have it on disk. Those differ: the Go goes inert and the
// tests come along, and installing the author's tree directly would test bytes
// nobody is ever served.
type RenderCmd struct {
	Dir string `arg:"" optional:"" name:"dir" help:"Plugin tree to render — the directory holding plugin.yaml. Default: the current directory. Pass a plugins/ directory with --catalogue."`
	Out string `name:"out" required:"" placeholder:"DIR" help:"Where to write the published form. REPLACED: a render that merged into a previous one's leftovers would publish a file the author has deleted."`

	Catalogue bool     `name:"catalogue" help:"Render every plugin under <dir> — the whole registry, as a publish rsyncs it. Refuses a directory holding no plugin, because rendering nothing is not a success and the publish deletes what is not rendered."`
	Notes     []string `name:"note" placeholder:"FILE" help:"With --catalogue: a file at the registry root to carry (repeatable). The root is otherwise bare and the publish deletes what is not rendered, so a note left out is a registry shipping without it."`
}

func (c *RenderCmd) Run() error {
	dir := c.Dir
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("plugin render: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("plugin render: %w", err)
	}

	if c.Catalogue {
		st, rerr := pluginrender.Catalogue(abs, c.Out, c.Notes)
		if rerr != nil {
			return rerr
		}
		fmt.Fprintf(core.Stdout, "plugin render: %d plugin(s) → %s\n", st.Plugins, c.Out)
		fmt.Fprintf(core.Stdout, "  %d file(s), %d Go (%d of them tests, which travel so a "+
			"reader outside the team can check the plugin)\n", st.Files, st.GoFiles, st.Tests)
		return nil
	}

	st, rerr := pluginrender.Plugin(abs, c.Out)
	if rerr != nil {
		return rerr
	}
	fmt.Fprintf(core.Stdout, "plugin render: %s → %s\n", filepath.Base(abs), c.Out)
	fmt.Fprintf(core.Stdout, "  %d file(s), %d Go (%d tests)\n", st.Files, st.GoFiles, st.Tests)
	return nil
}
