package consoleui

import (
	"embed"
	"io/fs"
	"net/http"
)

// distFS holds the built React+TS SPA. Slice 1 ships a placeholder
// index.html so the binary builds; slice 3 replaces web/dist with the
// real Vite build output (committed, so a plain `go build` needs no node
// toolchain).
//
//go:embed all:web/dist
var distFS embed.FS

// spaHandler serves the embedded SPA, falling back to index.html for any
// unknown path so client-side routing works.
func spaHandler() http.Handler {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		// Embedding guarantees web/dist exists; a failure here is a
		// build-time bug, so serve nothing rather than panic at runtime.
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve the asset if it exists; otherwise fall back to index.html
		// (SPA deep links / client routes).
		if r.URL.Path != "/" {
			if f, err := sub.Open(trimLeadingSlash(r.URL.Path)); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func trimLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
