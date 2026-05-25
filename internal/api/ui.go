package api

import (
	"io/fs"
	"net/http"

	webui "github.com/wallfacers/workhorse-agent/web"
)

// handleUI serves the embedded reference Web UI under /ui/. The middleware
// chain exempts this prefix from bearer auth and Origin enforcement so a
// browser opened against /ui works without extra headers.
func (s *Server) handleUI() http.Handler {
	sub, err := fs.Sub(webui.FS, ".")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "ui assets unavailable", http.StatusInternalServerError)
		})
	}
	fileSrv := http.FileServer(http.FS(sub))
	return http.StripPrefix("/ui/", fileSrv)
}
