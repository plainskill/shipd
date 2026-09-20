package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	configPath := flag.String("config", "/etc/shipd/config.json", "path to config JSON")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("shipd: config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Fatalf("shipd: data dir: %v", err)
	}
	if err := os.MkdirAll(cfg.BuildsDir(), 0o750); err != nil {
		log.Fatalf("shipd: builds dir: %v", err)
	}

	st, err := LoadState(cfg.StatePath())
	if err != nil {
		log.Fatalf("shipd: state: %v", err)
	}

	eng := NewEngine(cfg, st)
	if err := eng.EnsureNetwork(); err != nil {
		log.Fatalf("shipd: network: %v", err)
	}
	// reconcile: mark apps whose container is gone as stopped
	eng.Reconcile()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /check", eng.handleCheck)
	mux.Handle("GET /api/apps", eng.guard(handleAppsList(st)))
	mux.Handle("POST /api/deploy", eng.guard(http.HandlerFunc(eng.handleDeploy)))
	mux.Handle("POST /api/apps/{subdomain}/stop", eng.guard(eng.handleAction(actionStop)))
	mux.Handle("POST /api/apps/{subdomain}/start", eng.guard(eng.handleAction(actionStart)))
	mux.Handle("POST /api/apps/{subdomain}/redeploy", eng.guard(eng.handleAction(actionRedeploy)))
	mux.Handle("POST /api/apps/{subdomain}/delete", eng.guard(eng.handleAction(actionDelete)))
	mux.Handle("GET /api/apps/{subdomain}/logs", eng.guard(http.HandlerFunc(eng.handleLogs)))
	// token management (root or any active managed token)
	mux.Handle("GET /api/tokens", eng.guard(http.HandlerFunc(eng.handleTokensList)))
	mux.Handle("POST /api/tokens", eng.guard(http.HandlerFunc(eng.handleTokenCreate)))
	mux.Handle("POST /api/tokens/{id}/revoke", eng.guard(http.HandlerFunc(eng.handleTokenRevoke)))
	// dashboard: unauthenticated at this layer — the abm gate at Caddy is
	// the access control for the UI; the page authenticates API calls with
	// a token from localStorage
	mux.Handle("GET /{$}", http.HandlerFunc(eng.handleDashboard))

	srvs := []*http.Server{newServer(cfg.Listen, mux)}
	for _, addr := range cfg.ListenExtra {
		srvs = append(srvs, newServer(addr, mux))
	}
	for _, s := range srvs {
		log.Printf("shipd: listening on %s", s.Addr)
	}
	log.Printf("shipd: ask URL for Caddy on_demand_tls: http://172.16.0.1:8900/check?t=<redacted — see config.json>")
	log.Printf("shipd: zone %s (%d apps)", cfg.Domain, len(st.Apps))
	for _, s := range srvs[1:] {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil {
				log.Fatalf("shipd: %s: %v", s.Addr, err)
			}
		}(s)
	}
	log.Fatal(srvs[0].ListenAndServe())
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
