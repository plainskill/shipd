package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// version is stamped at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/shipd/config.json", "path to config JSON")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

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
	mux.Handle("GET /api/whoami", eng.guard(http.HandlerFunc(eng.handleWhoami)))
	mux.Handle("POST /api/delete", eng.guard(http.HandlerFunc(eng.handleDeleteByIdentity)))
	mux.Handle("POST /api/prune", eng.guard(http.HandlerFunc(eng.handlePrune)))
	mux.Handle("POST /api/tokens", eng.guard(http.HandlerFunc(eng.handleTokenCreate)))
	mux.Handle("POST /api/tokens/{id}/revoke", eng.guard(http.HandlerFunc(eng.handleTokenRevoke)))
	// dashboard endpoints: NO token auth at this layer — the abm gate at
	// Caddy is the access control for the UI. API tokens are exclusively
	// for programmatic deploys.
	mux.Handle("GET /dash/config", eng.gateOnly(http.HandlerFunc(eng.handleConfig)))
	mux.Handle("GET /dash/apps", eng.gateOnly(handleAppsList(st)))
	mux.Handle("POST /dash/deploy", eng.gateOnly(http.HandlerFunc(eng.handleDeploy)))
	mux.Handle("POST /dash/delete", eng.gateOnly(http.HandlerFunc(eng.handleDeleteByIdentity)))
	mux.Handle("POST /dash/apps/{subdomain}/stop", eng.gateOnly(eng.handleAction(actionStop)))
	mux.Handle("POST /dash/apps/{subdomain}/start", eng.gateOnly(eng.handleAction(actionStart)))
	mux.Handle("POST /dash/apps/{subdomain}/redeploy", eng.gateOnly(eng.handleAction(actionRedeploy)))
	mux.Handle("POST /dash/apps/{subdomain}/delete", eng.gateOnly(eng.handleAction(actionDelete)))
	mux.Handle("GET /dash/apps/{subdomain}/logs", eng.gateOnly(http.HandlerFunc(eng.handleLogs)))
	mux.Handle("GET /dash/tokens", eng.gateOnly(http.HandlerFunc(eng.handleTokensList)))
	mux.Handle("POST /dash/tokens", eng.gateOnly(http.HandlerFunc(eng.handleTokenCreate)))
	mux.Handle("POST /dash/tokens/{id}/revoke", eng.gateOnly(http.HandlerFunc(eng.handleTokenRevoke)))
	// dashboard page: unauthenticated by design (the authgate at Caddy
	// authenticates the human) but requires the Caddy-injected gate header
	// so sibling containers on shipd-net cannot reach it directly
	mux.Handle("GET /{$}", eng.gateOnly(http.HandlerFunc(eng.handleDashboard)))

	srvs := []*http.Server{newServer(cfg.Listen, mux)}
	for _, addr := range cfg.ListenExtra {
		srvs = append(srvs, newServer(addr, mux))
	}
	for _, s := range srvs {
		log.Printf("shipd: listening on %s", s.Addr)
	}
	log.Printf("shipd: ask URL for the proxy on_demand_tls: http://%s/check?t=<redacted — see config.json>", cfg.AskBase())
	log.Printf("shipd: version %s, zone %s (%d apps)", version, cfg.Domain, len(st.Apps))
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
