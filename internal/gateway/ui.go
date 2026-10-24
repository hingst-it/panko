package gateway

import (
	"embed"
	"net/http"
)

//go:embed ui/*
var uiFiles embed.FS

func serveUI(w http.ResponseWriter, r *http.Request) {
	file, media := "index.html", "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/ui":
	case "/ui/app.js":
		file, media = "app.js", "text/javascript; charset=utf-8"
	case "/ui/app.css":
		file, media = "app.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", media)
	data, _ := uiFiles.ReadFile("ui/" + file)
	_, _ = w.Write(data)
}
