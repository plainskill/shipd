package main

import (
	"embed"
	"net/http"
)

//go:embed dashboard.html
var dashFS embed.FS

// handleDashboard serves the embedded dashboard. Authentication is layered:
// abm (anti-bot) gates this vhost at Caddy; the page itself authenticates
// API calls with a user-supplied token stored in localStorage. GET / is
// intentionally unauthenticated at the shipd layer so the abm pass renders
// the UI instead of a Basic-auth wall.
func (e *Engine) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	raw, err := dashFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "dashboard missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(raw)
}
