package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Status lifecycle values for an App.
const (
	StatusQueued   = "queued"
	StatusBuilding = "building"
	StatusRunning  = "running"
	StatusFailed   = "failed"
	StatusStopped  = "stopped"
	StatusDeleting = "deleting"
)

// App is one deployed application, identified by repo+branch.
type App struct {
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch"`
	Subdomain  string    `json:"subdomain"`
	Domain     string    `json:"domain"`
	Image      string    `json:"image"`
	Port       int       `json:"port"`
	Status     string    `json:"status"`
	DesiredUp  bool      `json:"desired_up"`
	Container  string    `json:"container,omitempty"`
	GitSHA     string    `json:"git_sha,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastDeploy time.Time `json:"last_deploy"`
}

// State is shipd's entire persistence: one JSON file.
type State struct {
	mu   sync.RWMutex
	path string
	Apps map[string]*App `json:"apps"`
}

func LoadState(path string) (*State, error) {
	st := &State{path: path, Apps: map[string]*App{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return st, nil
}

// Save atomically persists the state file. Callers must NOT hold st.mu.
func (s *State) Save() {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return
	}
	if _, err := tmp.Write(raw); err == nil {
		_ = tmp.Chmod(0o600)
	}
	tmp.Close()
	if err == nil {
		_ = os.Rename(tmp.Name(), s.path)
	} else {
		os.Remove(tmp.Name())
	}
}

func (s *State) Get(key string) *App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.Apps[key]
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}

func appKey(repo, branch string) string { return repo + "|" + branch }

var slugStrip = regexp.MustCompile(`[^a-z0-9.-]+`)

// slug normalizes a string for use in image/container/router names.
func slug(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = slugStrip.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}
