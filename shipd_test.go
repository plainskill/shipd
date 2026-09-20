package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoinBlocksEscape(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "sub"), 0o755)
	os.WriteFile(filepath.Join(base, "sub", "Dockerfile"), []byte("FROM scratch"), 0o644)

	cases := []struct {
		rel  string
		want bool
	}{
		{".", true},
		{"sub", true},
		{"sub/Dockerfile", true},
		{"../..", false},
		{"../../etc", false},
		{"sub/../../etc", false},
		{"/etc/passwd", false},
		{"sub/../../../etc/passwd", false},
	}
	for _, c := range cases {
		_, ok := safeJoin(base, c.rel)
		if ok != c.want {
			t.Errorf("safeJoin(%q) = %v, want %v", c.rel, ok, c.want)
		}
	}
}

// a repo can ship a symlink pointing outside; a lexical check alone would pass it
func TestSafeJoinBlocksSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("host data"), 0o644)
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, ok := safeJoin(base, "link"); ok {
		t.Error("safeJoin allowed a symlink escaping the repo")
	}
	if _, ok := safeJoin(base, "link/secret"); ok {
		t.Error("safeJoin allowed a path through an escaping symlink")
	}
}

func TestValidRepoURL(t *testing.T) {
	good := []string{
		"https://forge.example.net/me/app",
		"https://forge.example.net/me/app.git",
		"git@forge.example.net:me/app.git",
		"ssh://git@forge.example.net:2222/me/app.git",
	}
	bad := []string{
		"",
		"forge.example.net/me/app",
		"https://user:pass@forge.example.net/me/app", // embedded credentials
		"https://forge.example.net",                  // no path
		"git@forge.example.net",
		"file:///etc/passwd",
	}
	for _, u := range good {
		if !validRepoURL(u) {
			t.Errorf("validRepoURL(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validRepoURL(u) {
			t.Errorf("validRepoURL(%q) = true, want false", u)
		}
	}
}

func TestValidBranchRejectsInjection(t *testing.T) {
	good := []string{"main", "feature/x", "release-1.2", "v1.0.0"}
	bad := []string{"", "-upload-pack=evil", "..", "a..b", "branch with space", "branch:colon"}
	for _, b := range good {
		if !validBranch(b) {
			t.Errorf("validBranch(%q) = false, want true", b)
		}
	}
	for _, b := range bad {
		if validBranch(b) {
			t.Errorf("validBranch(%q) = true, want false", b)
		}
	}
}

func TestValidSubdomain(t *testing.T) {
	good := []string{"hello", "excalidraw", "56131dc4488b", "a-b-c"}
	bad := []string{"", "Hello", "-lead", "trail-", "a_b", "a.b", "a/b", strings.Repeat("a", 64)}
	for _, s := range good {
		if err := validSubdomain(s); err != nil {
			t.Errorf("validSubdomain(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := validSubdomain(s); err == nil {
			t.Errorf("validSubdomain(%q) = nil, want error", s)
		}
	}
}

// app identity must be stable and repo URLs with/without .git must match
func TestAppKeyIgnoresGitSuffix(t *testing.T) {
	a := appKey("https://forge.example.net/me/app", "main")
	b := appKey(trimGitSuffix("https://forge.example.net/me/app.git"), "main")
	if a != b {
		t.Errorf("appKey differs for .git and non-.git forms: %s vs %s", a, b)
	}
	if a == appKey("https://forge.example.net/me/app", "dev") {
		t.Error("appKey ignores the branch")
	}
}

// two repos whose slugs collide must not share an image tag
func TestHashKeyDisambiguatesSlugs(t *testing.T) {
	k1 := "https://forge.example.net/me/a-b|main"
	k2 := "https://forge.example.net/me/a/b|main"
	if slug(k1) != slug(k2) {
		t.Skip("slugs no longer collide; test no longer meaningful")
	}
	if hashKey(k1) == hashKey(k2) {
		t.Error("hashKey collided for distinct keys with the same slug")
	}
}

func TestMergeEnvDeployTimeWins(t *testing.T) {
	repo := []string{"NODE_ENV=production", "PORT=3000"}
	got := mergeEnv(repo, map[string]string{"PORT": "8080", "API_KEY": "secret"})
	seen := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	if seen["PORT"] != "8080" {
		t.Errorf("deploy-time env did not override repo env: PORT=%q", seen["PORT"])
	}
	if seen["NODE_ENV"] != "production" {
		t.Errorf("repo env lost: NODE_ENV=%q", seen["NODE_ENV"])
	}
	if seen["API_KEY"] != "secret" {
		t.Errorf("new deploy-time key missing: %v", seen)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 entries, got %d: %v", len(got), got)
	}
}

func TestMergeEnvNoAppEnvPassesThrough(t *testing.T) {
	repo := []string{"A=1"}
	if got := mergeEnv(repo, nil); len(got) != 1 || got[0] != "A=1" {
		t.Errorf("mergeEnv with no app env = %v", got)
	}
}

// env values must never appear in the listing wire form
func TestViewAppMasksEnvValues(t *testing.T) {
	a := &App{
		Repo: "https://forge.example.net/me/app", Branch: "main",
		Subdomain: "app", Domain: "app.example.net", Status: StatusRunning,
		Env: map[string]string{"OPENAI_API_KEY": "sk-do-not-leak", "OTHER": "x"},
	}
	v := viewApp(a)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-do-not-leak") {
		t.Fatalf("appView leaked an env value: %s", raw)
	}
	if len(v.EnvKeys) != 2 || v.EnvKeys[0] != "OPENAI_API_KEY" {
		t.Errorf("env keys not reported (sorted): %v", v.EnvKeys)
	}
}

// prepareAppData is the persistence contract: the app's /data directory exists,
// is group-writable (a non-root image user writes through --group-add), and the
// by-name symlink exists so the hash-keyed directory is findable. A previous
// edit dropped the symlink call, leaving the helper defined but never invoked.
func TestPrepareAppDataCreatesDirAndNameLink(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{cfg: &Config{DataDir: dir}}
	app := &App{Repo: "https://forge.example.net/me/app", Branch: "main", Subdomain: "demo"}

	dataDir, gid, err := e.prepareAppData(app)
	if err != nil {
		t.Fatal(err)
	}
	if gid == "" {
		t.Error("gid must be returned for --group-add")
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("data dir is not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o775 {
		t.Errorf("data dir mode = %o, want 775 (group-writable so the container user can write)", perm)
	}

	link := filepath.Join(dir, "apps", "by-name", "demo")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("by-name symlink missing (dead-code regression): %v", err)
	}
	if target != dataDir {
		t.Errorf("by-name link points at %q, want %q", target, dataDir)
	}

	// idempotent: a redeploy must not fail or duplicate anything
	if _, _, err := e.prepareAppData(app); err != nil {
		t.Errorf("second call failed: %v", err)
	}
}

// the data directory is keyed by app identity, so a subdomain change keeps the
// same storage
func TestPrepareAppDataKeyedByIdentityNotSubdomain(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{cfg: &Config{DataDir: dir}}

	first := &App{Repo: "https://forge.example.net/me/app", Branch: "main", Subdomain: "alpha"}
	d1, _, err := e.prepareAppData(first)
	if err != nil {
		t.Fatal(err)
	}
	renamed := &App{Repo: "https://forge.example.net/me/app", Branch: "main", Subdomain: "beta"}
	d2, _, err := e.prepareAppData(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("subdomain change moved the data dir: %s vs %s", d1, d2)
	}
	// a different branch is a different app, so it gets its own storage
	other := &App{Repo: "https://forge.example.net/me/app", Branch: "dev", Subdomain: "alpha"}
	d3, _, err := e.prepareAppData(other)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Error("a different branch must not share the data dir")
	}
}
