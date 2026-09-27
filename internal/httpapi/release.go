package httpapi

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"

	"agent-gateway/internal/release"
)

//go:embed install.sh
var signedInstallerSH string

//go:embed install.ps1
var signedInstallerPS string

func (s *Server) handleReleaseFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	allowed := name == "manifest.json" || name == "manifest.sig"
	for _, n := range release.Targets {
		allowed = allowed || n == name
	}
	for _, n := range release.NoticeFiles {
		allowed = allowed || n == name
	}
	if !allowed || s.dataDir == "" {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(filepath.Join(s.dataDir, "dist"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, stat.ModTime(), f)
}
