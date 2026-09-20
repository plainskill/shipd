package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is the shipd control-plane configuration, loaded from JSON.
type Config struct {
	Listen string `json:"listen"` // primary listen address

	// ListenExtra are additional listen addresses. Used to expose the API on a
	// docker bridge gateway IP so a containerized reverse proxy (the edge
	// proxy) can reach the host process. Not publicly exposed, BUT every app
	// container on the app network can reach this address too — which is why
	// the dashboard paths require the edge-injected X-Shipd-Gate header.
	ListenExtra []string `json:"listen_extra"`

	// Domain is the app zone (required): apps are served at
	// <subdomain>.<domain>. There is deliberately no default — a wrong
	// default would silently publish into someone else's zone.
	Domain string `json:"domain"`

	// Registry is the optional image registry, e.g. "registry.example:5000".
	// Empty means local-only images and no push attempt (a deployment without
	// a registry should not log push failures).
	Registry string `json:"registry"`

	// Network is the docker network app containers join (default "shipd-net").
	Network string `json:"network"`

	// NetworkSubnet, when set, is used if the network has to be created
	// (e.g. "172.16.0.0/24"). Pin it when something else — a reverse proxy, a
	// firewall rule — depends on the network's gateway address.
	NetworkSubnet string `json:"network_subnet"`

	// SourceURL is an optional link shown in the dashboard (e.g. this
	// deployment's forge page). Omitted from the UI when empty.
	SourceURL string `json:"source_url"`

	// EdgeProbe is an optional URL template shipd polls after promoting a
	// container, so a deploy is only reported successful once the app is
	// reachable *through the edge*. "{domain}" is replaced with the app's
	// domain, e.g. "https://{domain}/". Empty disables the check. It closes
	// the window where the container is healthy but the edge has not yet
	// discovered its router (a 404 from the proxy for a few seconds).
	EdgeProbe string `json:"edge_probe"`

	APIToken string `json:"api_token"`
	DataDir  string `json:"data_dir"`

	// Reserved subdomains can never be claimed by a deployed app.
	Reserved []string `json:"reserved"`
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := &Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Domain == "" {
		return nil, fmt.Errorf("domain is required in %s (apps are served at <subdomain>.<domain>)", path)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/data"
	}
	if cfg.Network == "" {
		cfg.Network = "shipd-net"
	}
	if cfg.APIToken == "" {
		return nil, fmt.Errorf("api_token is required in %s", path)
	}
	if len(cfg.Reserved) == 0 {
		// generic, deployment-agnostic defaults; add your own names explicitly
		cfg.Reserved = []string{"shipd", "api", "check", "healthz", "dashboard", "www", "proxy", "gate", "login", "admin"}
	}
	return cfg, nil
}

// StatePath returns the path of the JSON state file.
func (c *Config) StatePath() string { return filepath.Join(c.DataDir, "state.json") }

// AllowlistPath returns the path of the on-demand TLS domain allowlist.
func (c *Config) AllowlistPath() string { return filepath.Join(c.DataDir, "domains.json") }

// BuildsDir returns the directory holding repo working copies.
func (c *Config) BuildsDir() string { return filepath.Join(c.DataDir, "builds") }

// trimGitSuffix strips whitespace and a trailing ".git" from a repo URL.
func trimGitSuffix(s string) string {
	return strings.TrimSuffix(strings.TrimSpace(s), ".git")
}

// AppsDir is where per-app persistent data lives. It is bind-mounted to /data
// inside every app container, so a redeploy does not wipe app state (SQLite
// files, uploads, caches).
func (c *Config) AppsDir() string { return filepath.Join(c.DataDir, "apps") }

// AppDataDir returns the persistent data directory for an app key.
func (c *Config) AppDataDir(key string) string {
	return filepath.Join(c.AppsDir(), hashKey(key))
}

// AskBase is the address an edge proxy should use to reach shipd for the
// on-demand TLS ask endpoint. It prefers an explicit listen_extra address
// (typically the app network's gateway) and falls back to the primary listen
// address.
func (c *Config) AskBase() string {
	if len(c.ListenExtra) > 0 {
		return c.ListenExtra[0]
	}
	return c.Listen
}

// AskToken derives the static token the edge proxy presents to /check. Derived from
// the API token so there is exactly one secret to manage.
func (c *Config) AskToken() string {
	h := sha256.Sum256([]byte("shipd-ask:" + c.APIToken))
	return hex.EncodeToString(h[:16])
}

// GateToken derives the shared secret the edge proxy injects as X-Shipd-Gate
// on the dashboard paths. /dash/* is unauthenticated by design (the
// authenticating edge authenticates humans), so this header is what
// distinguishes a request that came through the edge from one issued by a
// sibling app container on the app network.
func (c *Config) GateToken() string {
	h := sha256.Sum256([]byte("shipd-gate:" + c.APIToken))
	return hex.EncodeToString(h[:16])
}

// validSubdomain enforces a single DNS label, lowercase, sane charset.
func validSubdomain(s string) error {
	if s == "" {
		return fmt.Errorf("subdomain is required")
	}
	if len(s) > 63 {
		return fmt.Errorf("subdomain too long")
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return fmt.Errorf("subdomain must be lowercase alnum and dashes")
		}
	}
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return fmt.Errorf("subdomain cannot start or end with a dash")
	}
	return nil
}

// validBranch rejects git option injection: must look like a ref name,
// no leading dash, no "..".
func validBranch(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	if strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '/' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// validRepoURL accepts https:// and git@ (scp-style) git URLs, with or
// without ".git". Rejects embedded credentials.
func validRepoURL(s string) bool {
	if strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		return err == nil && u.Host != "" && strings.Contains(u.Path, "/") && u.User == nil
	}
	if strings.HasPrefix(s, "ssh://") {
		// ssh://[user@]host[:port]/owner/repo(.git) — user (git@) and port are
		// both normal here; a password in the URL is not
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || !strings.Contains(u.Path, "/") {
			return false
		}
		if u.User == nil {
			return true
		}
		_, hasPassword := u.User.Password()
		return !hasPassword
	}
	if strings.HasPrefix(s, "git@") {
		// scp-like: git@host:owner/repo(.git) — no leading slash
		rest := strings.TrimPrefix(s, "git@")
		host, path, ok := strings.Cut(rest, ":")
		return ok && host != "" && path != "" && !strings.Contains(path, "//")
	}
	return false
}
