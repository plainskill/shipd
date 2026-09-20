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
	mux.Handle("GET /", eng.guard(http.HandlerFunc(eng.handleDashboard)))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("shipd: listening on %s (domain=%s apps=%d)", cfg.Listen, cfg.Domain, len(st.Apps))
	log.Fatal(srv.ListenAndServe())
}
