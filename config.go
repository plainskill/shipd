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

	// ListenExtra are additional listen addresses. Used to expose the API on
	// a docker bridge gateway IP (e.g. 172.16.0.1:8900, the shipd-net
	// gateway) so a containerized reverse proxy (Caddy) can reach the host
	// process. Never exposed publicly (UFW default-deny covers non-loopback),
	// BUT every app container on shipd-net can also reach this address — so
	// the dashboard paths require the Caddy-injected X-Shipd-Gate header.
	ListenExtra []string `json:"listen_extra"`

	// Domain is the app zone (default is this deployment's; set it explicitly
	// on any other host).
	Domain string `json:"domain"`

	// Registry is where built images are tagged/pushed. The push is
	// best-effort; an unreachable registry degrades to local-only images.
	Registry string `json:"registry"`

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
		cfg.Domain = "apps.plainskill.net"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/data"
	}
	if cfg.Registry == "" {
		cfg.Registry = "localhost:5000"
	}
	if cfg.APIToken == "" {
		return nil, fmt.Errorf("api_token is required in %s", path)
	}
	if len(cfg.Reserved) == 0 {
		cfg.Reserved = []string{"shipd", "api", "check", "healthz", "dashboard", "proxy", "caddy", "forgejo", "git", "mail", "smtp", "ns1", "ns2", "vpn", "status"}
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

// AskBase is the address a reverse proxy should use to reach shipd for the
// on-demand TLS ask endpoint. It prefers an explicit listen_extra address
// (the docker bridge gateway in this deployment) and falls back to the
// primary listen address.
func (c *Config) AskBase() string {
	if len(c.ListenExtra) > 0 {
		return c.ListenExtra[0]
	}
	return c.Listen
}

// AskToken derives the static token Caddy presents to /check. Derived from
// the API token so there is exactly one secret to manage.
func (c *Config) AskToken() string {
	h := sha256.Sum256([]byte("shipd-ask:" + c.APIToken))
	return hex.EncodeToString(h[:16])
}

// GateToken derives the shared secret Caddy injects as X-Shipd-Gate on the
// dashboard paths. /dash/* is unauthenticated by design (the authgate at
// Caddy authenticates users), so this header is what distinguishes a request
// that came through Caddy from one issued by a sibling container on shipd-net.
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
