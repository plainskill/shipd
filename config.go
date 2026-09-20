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
	// a docker bridge gateway IP (e.g. 172.30.0.1:8900) so a containerized
	// reverse proxy (Caddy) can reach the host process. Never exposed
	// publicly: UFW default-deny covers non-loopback, and the address only
	// exists on shipd-net.
	ListenExtra []string `json:"listen_extra"`

	Domain   string `json:"domain"` // e.g. apps.plainskill.net
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

// AskToken derives the static token Caddy presents to /check. Derived from
// the API token so there is exactly one secret to manage.
func (c *Config) AskToken() string {
	h := sha256.Sum256([]byte("shipd-ask:" + c.APIToken))
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

// validRepoURL accepts https:// and git@ git URLs, with or without ".git".
func validRepoURL(s string) bool {
	if strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		return err == nil && u.Host != "" && strings.Contains(u.Path, "/")
	}
	if strings.HasPrefix(s, "git@") {
		// git@host:path/to/repo(.git)
		rest := strings.TrimPrefix(s, "git@")
		host, path, ok := strings.Cut(rest, ":")
		return ok && host != "" && strings.HasPrefix(path, "/")
	}
	return false
}
