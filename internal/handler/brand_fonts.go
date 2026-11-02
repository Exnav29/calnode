package handler

import (
	"net/http"
	"os"
	"path/filepath"
)

// brandFonts maps the public names under /brand-fonts/ to files in DATA_DIR/fonts. They are
// Fontshare fonts (ITF Free Font License): self-hosting on our own site is allowed, but the
// files may not be redistributed, so they live on the server's data volume and never in
// this public repository. Only these names are served; a missing file is a 404 and the
// pages fall back to their system font stacks.
var brandFonts = map[string]string{
	"satoshi.woff2":  "Satoshi-Variable.woff2",
	"sentient.woff2": "Sentient-Variable.woff2",
}

// ServeBrandFont handles GET /brand-fonts/{file}.
func (h *Handler) ServeBrandFont(w http.ResponseWriter, r *http.Request) {
	name, ok := brandFonts[r.PathValue("file")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(h.dataDir, "fonts", name)
	f, err := os.Open(path) // #nosec G304 -- name comes from the fixed brandFonts map, never user input
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
