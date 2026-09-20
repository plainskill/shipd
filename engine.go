package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	logTimeout       = 30 * time.Second
	healthTimeout    = 45 * time.Second
	healthInterval   = 2 * time.Second
	edgeProbeTimeout = 40 * time.Second
)

// Engine executes deploys and docker/git operations against the host.
type Engine struct {
	cfg *Config
	st  *State

	// mu serializes deploy pipelines AND stop/delete (single-user box; one
	// mutating operation at a time so state and containers can't diverge).
	mu sync.Mutex
}

func NewEngine(cfg *Config, st *State) *Engine {
	return &Engine{cfg: cfg, st: st}
}

type action string

const (
	actionStop     action = "stop"
	actionStart    action = "start"
	actionRedeploy action = "redeploy"
	actionDelete   action = "delete"
)

// ---- state helpers ---------------------------------------------------

// Key returns the app identity: repo|branch.
func (a *App) Key() string { return appKey(a.Repo, a.Branch) }

// hashKey returns a short collision-resistant hash of the app key, used in
// image tags and build dirs so slug-folded repos can't cross-wire.
func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:8]
}

// containerName is the stable name of the app's running container.
func (a *App) containerName() string { return "shipd-app-" + a.Subdomain }

// tempContainerName is used while a new version is being health-checked.
func (a *App) tempContainerName(sha string) string {
	return "shipd-new-" + a.Subdomain + "-" + short(sha)
}

// Update mutates one app under lock and persists state.
func (s *State) Update(key string, fn func(a *App)) {
	s.mu.Lock()
	a := s.Apps[key]
	if a == nil {
		s.mu.Unlock()
		return
	}
	fn(a)
	s.mu.Unlock()
	s.Save()
}

// ---- process helpers ---------------------------------------------------

func (e *Engine) runDocker(timeout time.Duration, args ...string) (string, error) {
	return e.runCmdEnv(timeout, "docker", args...)
}

// runCmdEnv runs a command with a writable HOME under the shipd data dir
// (systemd ProtectHome=tmpfs leaves no usable HOME for git/docker buildx).
func (e *Engine) runCmdEnv(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	home := filepath.Join(e.cfg.DataDir, "home")
	if err := os.MkdirAll(home, 0o750); err != nil {
		log.Printf("shipd: warning: cannot create HOME %s: %v", home, err)
	}
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ---- state operations ---------------------------------------------------

// Upsert creates or updates the app record for repo+branch with subdomain.
func (e *Engine) Upsert(repo, branch, subdomain string) *App {
	key := appKey(repo, branch)
	e.st.mu.Lock()
	now := time.Now().UTC()
	a := e.st.Apps[key]
	if a == nil {
		a = &App{
			Repo:      repo,
			Branch:    branch,
			Subdomain: subdomain,
			CreatedAt: now,
		}
		e.st.Apps[key] = a
	}
	a.Subdomain = subdomain
	a.Domain = subdomain + "." + e.cfg.Domain
	// hashKey disambiguates slug-folded repos (https://x/a-b vs https://x/a/b)
	base := "shipd/" + slug(repo) + "-" + hashKey(key) + ":" + slug(branch)
	if e.cfg.Registry != "" {
		base = e.cfg.Registry + "/" + base
	}
	a.Image = base
	a.Status = StatusQueued
	a.DesiredUp = true
	a.LastDeploy = now
	cp := *a
	e.st.mu.Unlock()
	e.st.Save()
	return &cp
}

func (e *Engine) BySubdomain(sub string) *App {
	e.st.mu.RLock()
	defer e.st.mu.RUnlock()
	for _, a := range e.st.Apps {
		if a.Subdomain == sub {
			cp := *a
			return &cp
		}
	}
	return nil
}

// randomSubdomain mints an unused 12-hex-char subdomain (48 bits).
func (e *Engine) randomSubdomain() (string, error) {
	for i := 0; i < 8; i++ {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		sub := hex.EncodeToString(b)
		if e.reserved(sub) || e.BySubdomain(sub) != nil {
			continue
		}
		return sub, nil
	}
	return "", fmt.Errorf("no free subdomain after 8 attempts")
}

func (e *Engine) reserved(sub string) bool {
	for _, r := range e.cfg.Reserved {
		if r == sub {
			return true
		}
	}
	return false
}

// DomainAllowed gates Caddy on-demand TLS: only provisioned app domains (and
// the zone apex itself) may receive certificates.
func (e *Engine) DomainAllowed(domain string) bool {
	if domain == e.cfg.Domain {
		return true
	}
	e.st.mu.RLock()
	defer e.st.mu.RUnlock()
	for _, a := range e.st.Apps {
		if a.Domain == domain {
			return true
		}
	}
	return false
}

// EnsureNetwork creates the shipd-net docker network if missing.
func (e *Engine) EnsureNetwork() error {
	name := e.cfg.Network
	out, err := e.runDocker(30*time.Second, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return fmt.Errorf("network ls: %v: %s", err, out)
	}
	for _, n := range strings.Fields(out) {
		if n == name {
			return nil
		}
	}
	args := []string{"network", "create"}
	if e.cfg.NetworkSubnet != "" {
		// pin the subnet: anything depending on the gateway address (a proxy's
		// upstream, a firewall rule) breaks if docker picks a random one
		args = append(args, "--subnet", e.cfg.NetworkSubnet)
	}
	args = append(args, name)
	if out, err := e.runDocker(time.Minute, args...); err != nil {
		return fmt.Errorf("network create: %v: %s", err, out)
	}
	log.Printf("shipd: created network %s (subnet %q)", name, e.cfg.NetworkSubnet)
	return nil
}

// Reconcile aligns status with reality (e.g. after a host reboot) and cleans
// up temp containers orphaned by a restart mid-deploy.
// setEnv merges deploy-time environment into an app and removes any keys named
// in unset. An empty value is stored as an empty value (a dotenv file's
// KEY= means exactly that); use unset to remove a key. Values live in shipd's
// state (0600), never in the repo.
func (e *Engine) setEnv(key string, env map[string]string, unset []string) *App {
	e.st.mu.Lock()
	a := e.st.Apps[key]
	if a == nil {
		e.st.mu.Unlock()
		return nil
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	for k, v := range env {
		a.Env[k] = v
	}
	for _, k := range unset {
		delete(a.Env, k)
	}
	e.st.mu.Unlock()
	e.st.Save()
	return e.st.Get(key)
}

// PruneResult reports what a prune reclaimed.
type PruneResult struct {
	BuildDirs []string `json:"build_dirs"`
	Images    []string `json:"images"`
	DataDirs  []string `json:"orphan_data_dirs"` // reported, never deleted
}

// Prune removes build directories and images belonging to apps that are no
// longer in state. Orphaned app data directories are only *reported* — app
// data is never deleted without an explicit request.
//
// Images are scoped to <registry>/shipd/ so nothing else on the host can be
// touched, and every removal is by explicit tag (never a blanket prune).
func (e *Engine) Prune() PruneResult {
	var res PruneResult
	live := map[string]bool{}
	e.st.mu.RLock()
	for k := range e.st.Apps {
		live[hashKey(k)] = true
	}
	e.st.mu.RUnlock()

	// build dirs: named <slug>-<hash8>
	entries, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "builds"))
	if err == nil {
		for _, en := range entries {
			name := en.Name()
			parts := strings.Split(name, "-")
			if len(parts) < 2 {
				continue
			}
			h := parts[len(parts)-1]
			if live[h] {
				continue
			}
			p := filepath.Join(e.cfg.DataDir, "builds", name)
			if os.RemoveAll(p) == nil {
				res.BuildDirs = append(res.BuildDirs, name)
			}
		}
	}

	// images: only <registry>/shipd/* tags
	prefix := "shipd/"
	if e.cfg.Registry != "" {
		prefix = e.cfg.Registry + "/shipd/"
	}
	out, _ := e.runDocker(time.Minute, "images", "--format", "{{.Repository}}:{{.Tag}}")
	for _, img := range strings.Fields(out) {
		if !strings.HasPrefix(img, prefix) || strings.Contains(img, "<none>") {
			continue
		}
		// keep anything whose hash matches a live app, or that a container uses
		keep := false
		for h := range live {
			if strings.Contains(img, "-"+h+":") {
				keep = true
				break
			}
		}
		if keep {
			continue
		}
		if _, err := e.runDocker(time.Minute, "rmi", img); err == nil {
			res.Images = append(res.Images, img)
		}
	}

	// orphaned data dirs (reported only)
	dirs, err := os.ReadDir(e.cfg.AppsDir())
	if err == nil {
		for _, en := range dirs {
			name := en.Name()
			if name == "by-name" || !en.IsDir() {
				continue
			}
			if !live[name] {
				res.DataDirs = append(res.DataDirs, filepath.Join(e.cfg.AppsDir(), name))
			}
		}
	}
	return res
}

func (e *Engine) Reconcile() {
	// orphaned temps from a mid-deploy crash
	if out, _ := e.runDocker(30*time.Second, "ps", "-aq", "--filter", "name=shipd-new-"); out != "" {
		for _, id := range strings.Fields(out) {
			_, _ = e.runDocker(time.Minute, "rm", "-f", id)
		}
		log.Println("shipd: cleaned orphaned temp containers")
	}
	type ent struct {
		key     string
		desired bool
		sub     string
		cont    string
	}
	e.st.mu.RLock()
	var entries []ent
	for k, a := range e.st.Apps {
		entries = append(entries, ent{k, a.DesiredUp, a.Subdomain, a.containerName()})
	}
	e.st.mu.RUnlock()
	for _, en := range entries {
		out, _ := e.runDocker(20*time.Second, "ps", "-q", "--filter", "name=^/"+en.cont+"$")
		if strings.TrimSpace(out) != "" {
			e.st.Update(en.key, func(a *App) { a.Status = StatusRunning })
		} else {
			e.st.Update(en.key, func(a *App) { a.Status = StatusStopped })
			if en.desired {
				log.Printf("shipd: %s desired up but container missing; marked stopped (redeploy or start)", en.sub)
			}
		}
	}
}

// shipdJSON is the optional per-repo build/deploy descriptor.
type shipdJSON struct {
	Port       int      `json:"port"`
	Dockerfile string   `json:"dockerfile"`
	Context    string   `json:"context"`
	Env        []string `json:"env"`
}

func (e *Engine) readDeck(src string) shipdJSON {
	var m shipdJSON
	if raw, err := os.ReadFile(filepath.Join(src, "shipd.json")); err == nil {
		if json.Unmarshal(raw, &m) != nil {
			log.Printf("shipd: warning: invalid shipd.json in %s (ignored)", src)
			return shipdJSON{}
		}
	}
	return m
}

// deckPaths resolves the build context dir and Dockerfile path from a deck.
// Dockerfile may be relative to the repo root; context may be a subdir.
func deckPaths(src string, deck shipdJSON) (ctxDir, dockerfile string) {
	ctxDir = src
	if deck.Context != "" {
		if p, ok := safeJoin(src, deck.Context); ok {
			ctxDir = p
		} else {
			log.Printf("shipd: rejecting shipd.json context %q (escapes repo)", deck.Context)
		}
	}
	dockerfile = filepath.Join(src, "Dockerfile")
	if deck.Dockerfile != "" {
		if p, ok := safeJoin(src, deck.Dockerfile); ok {
			dockerfile = p
		} else {
			log.Printf("shipd: rejecting shipd.json dockerfile %q (escapes repo)", deck.Dockerfile)
		}
	}
	return ctxDir, dockerfile
}

// safeJoin joins rel onto base and reports whether the result stays inside
// base (blocks "context": "../.." style escapes in a hostile shipd.json).
func safeJoin(base, rel string) (string, bool) {
	if filepath.IsAbs(rel) {
		// filepath.Join would re-root an absolute path inside base, which is
		// safe but wrong: a repo declaring an absolute context is confused
		return "", false
	}
	p := filepath.Clean(filepath.Join(base, rel))
	base = filepath.Clean(base)
	if p != base && !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", false
	}
	// lexical checks miss symlinks: a repo can ship "context -> /etc" and the
	// build would happily COPY from the resolved path
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p, true // does not exist yet; docker will report it
	}
	rbase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return p, true
	}
	if real != rbase && !strings.HasPrefix(real, rbase+string(os.PathSeparator)) {
		return "", false
	}
	return p, true
}

// checkout clones (depth 1) or updates a repo working copy at dst.
// Runs through runCmdEnv so git sees HOME=<data>/home — that is where deploy
// keys (~/.ssh) and git config must live for git@ / private repos to work.
// A failed clone removes dst so the next deploy starts clean (a partial
// clone without .git would otherwise wedge the fetch path forever).
func (e *Engine) checkout(repo, branch, dst string) (string, error) {
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		out, err := e.runCmdEnv(5*time.Minute, "git", "clone", "--depth", "1", "--branch", branch,
			"--single-branch", repo, dst)
		if err != nil {
			os.RemoveAll(dst)
			return out, fmt.Errorf("clone %s@%s: %v: %s", repo, branch, err, tail(out, 200))
		}
	} else {
		if out, err := e.runCmdEnv(time.Minute, "git", "-C", dst, "fetch", "--depth", "1", "origin", branch); err != nil {
			return out, fmt.Errorf("fetch: %v: %s", err, tail(out, 200))
		}
		if out, err := e.runCmdEnv(time.Minute, "git", "-C", dst, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return out, fmt.Errorf("checkout: %v: %s", err, tail(out, 200))
		}
	}
	return e.runCmdEnv(15*time.Second, "git", "-C", dst, "rev-parse", "HEAD")
}

// RunDeploy is the pipeline: checkout -> build -> probe temp (unrouted) ->
// promote to stable (routed). The temp container carries NO traefik router
// labels, so unvalidated code never serves live traffic; on probe failure
// the old version keeps serving untouched.
func (e *Engine) RunDeploy(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	app := e.st.Get(key)
	if app == nil {
		return
	}
	src := filepath.Join(e.cfg.BuildsDir(), slug(app.Repo)+"-"+hashKey(key))
	started := time.Now()

	fail := func(err error) {
		log.Printf("shipd: deploy %s FAILED: %v", app.Subdomain, err)
		e.st.Update(key, func(a *App) {
			a.Status = StatusFailed
			a.LastError = err.Error()
		})
	}

	e.st.Update(key, func(a *App) { a.Status = StatusBuilding })

	// 1. checkout
	sha, err := e.checkout(app.Repo, app.Branch, src)
	if err != nil {
		fail(err)
		return
	}
	e.st.Update(key, func(a *App) { a.GitSHA = short(sha) })
	log.Printf("shipd: %s building %s", app.Subdomain, short(sha))

	// 2. resolve port: shipd.json -> EXPOSE (in the deck-resolved Dockerfile)
	deck := e.readDeck(src)
	ctxDir, dockerfile := deckPaths(src, deck)
	port := deck.Port
	if port == 0 {
		port = exposedPort(dockerfile)
	}
	if port == 0 {
		fail(fmt.Errorf("no listen port: add shipd.json {\"port\": N} or EXPOSE in Dockerfile"))
		return
	}

	// 3. build
	image := app.Image
	buildArgs := []string{
		"build",
		"-f", dockerfile,
		"-t", image,
		"--network", "host",
		"--label", "shipd.app=" + key,
		"--label", "shipd.subdomain=" + app.Subdomain,
		ctxDir,
	}
	if out, err := e.runDocker(30*time.Minute, buildArgs...); err != nil {
		fail(fmt.Errorf("build: %v: %s", err, tail(out, 500)))
		return
	}

	// 4. push (best-effort; the local image is what actually runs). Skipped
	// entirely when no registry is configured.
	if e.cfg.Registry != "" {
		if out, err := e.runDocker(5*time.Minute, "push", image); err != nil {
			log.Printf("shipd: push %s failed (continuing with local image): %v: %s", image, err, tail(out, 200))
		}
	}

	// 5. run new version under a temp name WITHOUT router labels — it must
	// not receive live traffic until validated
	temp := app.tempContainerName(sha)
	_, _ = e.runDocker(time.Minute, "rm", "-f", temp) // stale same-sha container
	runArgs := []string{
		"run", "-d", "--name", temp,
		"--network", e.cfg.Network,
		"--restart", "unless-stopped",
		"--label", "shipd.app=" + key,
		"--label", "shipd.subdomain=" + app.Subdomain,
		"--label", "shipd.port=" + fmt.Sprint(port),
	}
	for _, env := range deck.Env {
		runArgs = append(runArgs, "--env", env)
	}
	runArgs = append(runArgs, image)
	if out, err := e.runDocker(2*time.Minute, runArgs...); err != nil {
		fail(fmt.Errorf("run: %v: %s", err, tail(out, 300)))
		return
	}

	// 6. health check the temp directly by IP; on failure remove it — the
	// old version never stopped serving
	if !e.waitHealthy(temp, healthTimeout) {
		state, _ := e.runDocker(10*time.Second, "inspect", "-f", "{{.State.Status}}", temp)
		logs, _ := e.runDocker(15*time.Second, "logs", "--tail", "20", temp)
		_, _ = e.runDocker(time.Minute, "rm", "-f", temp)
		fail(fmt.Errorf("health check failed (state=%s); rolled back. recent logs: %s",
			strings.TrimSpace(state), tail(strings.ReplaceAll(logs, "\n", " | "), 300)))
		return
	}

	// 7. promote. Re-check state first: a delete/stop may have landed while
	// we were building.
	current := e.st.Get(key)
	if current == nil {
		_, _ = e.runDocker(time.Minute, "rm", "-f", temp)
		log.Printf("shipd: %s deleted during deploy; discarding build", app.Subdomain)
		return
	}
	if !current.DesiredUp {
		_, _ = e.runDocker(time.Minute, "rm", "-f", temp)
		e.st.Update(key, func(a *App) { a.Status = StatusStopped })
		log.Printf("shipd: %s stopped during deploy; discarding build", app.Subdomain)
		return
	}

	// remove the old stable + any orphaned containers for this app key
	// (covers subdomain reassignment: old-name containers carry the same
	// shipd.app label)
	e.removeAppContainers(key, temp)

	// run the validated image under the stable name WITH router labels
	if err := e.runContainer(current, image, port, deck.Env); err != nil {
		fail(fmt.Errorf("promote: %v", err))
		_, _ = e.runDocker(time.Minute, "rm", "-f", temp)
		return
	}
	if !e.waitHealthy(current.containerName(), 15*time.Second) {
		// container stays up (Traefik routes to it) but the deploy is
		// reported failed — do NOT fall through and overwrite that
		fail(fmt.Errorf("promoted container failed immediate probe; check logs"))
		return
	}
	_, _ = e.runDocker(time.Minute, "rm", "-f", temp)

	if err := e.waitEdgeRouted(current, edgeProbeTimeout); err != nil {
		// the container is healthy; a slow or misconfigured edge should be
		// loud, not a silent "deployed"
		log.Printf("shipd: WARNING: %v", err)
	}

	e.st.Update(key, func(a *App) {
		a.Status = StatusRunning
		a.Port = port
		a.Container = current.containerName()
		a.LastError = ""
		a.LastDeploy = time.Now().UTC()
	})
	log.Printf("shipd: %s deployed %s in %s", app.Subdomain, short(sha), time.Since(started).Round(time.Second))
}

// removeAppContainers removes every container labeled for this app key
// except the one named in keep.
func (e *Engine) removeAppContainers(key, keep string) {
	out, _ := e.runDocker(30*time.Second, "ps", "-aq", "--filter", "label=shipd.app="+key)
	for _, id := range strings.Fields(out) {
		// ps returns IDs, keep is a name — resolve before comparing, or the
		// keep parameter silently keeps nothing
		if keep != "" && e.containerName(id) == keep {
			continue
		}
		e.runDocker(30*time.Second, "rm", "-f", id)
	}
}

// linkAppDataName maintains <apps>/by-name/<subdomain> -> <apps>/<hash> so the
// data dir is findable by name while the storage stays keyed by app identity
// (which survives subdomain changes).
func (e *Engine) linkAppDataName(app *App) error {
	byName := filepath.Join(e.cfg.AppsDir(), "by-name")
	if err := os.MkdirAll(byName, 0o755); err != nil {
		return fmt.Errorf("create by-name dir: %w", err)
	}
	link := filepath.Join(byName, app.Subdomain)
	target := e.cfg.AppDataDir(app.Key())
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("link app data %s: %w", app.Subdomain, err)
	}
	return nil
}

// waitEdgeRouted polls the configured edge URL until the app answers through
// it, so "deployed" means reachable and not merely "container running". Any
// response other than 404 counts as routed (the proxy returns 404 when no
// router matches the host). Disabled when edge_probe is unset.
func (e *Engine) waitEdgeRouted(app *App, timeout time.Duration) error {
	if e.cfg.EdgeProbe == "" {
		return nil
	}
	target := strings.ReplaceAll(e.cfg.EdgeProbe, "{domain}", app.Domain)
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return fmt.Errorf("edge_probe url %q: %w", target, err)
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			last = err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				return nil
			}
			last = "HTTP 404 (no router matched yet)"
		}
		time.Sleep(700 * time.Millisecond)
	}
	return fmt.Errorf("edge did not route %s within %s (last: %s) — container is up, check the proxy", target, timeout, last)
}

// containerName resolves a container ID to its name, without the leading slash.
func (e *Engine) containerName(id string) string {
	out, err := e.runDocker(15*time.Second, "inspect", "--format", "{{.Name}}", id)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "/")
}

// waitHealthy probes http://<ip>:<port>/ until it answers or timeout.
// Any HTTP response counts as healthy (the app is up; a 500 is an app-level
// problem that logs will show). No shipd.port label = skip probing.
func (e *Engine) waitHealthy(container string, within time.Duration) bool {
	port := e.portOf(container)
	if port == 0 {
		log.Printf("shipd: %s has no shipd.port label; skipping HTTP probe", container)
		return true
	}
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ip, err := e.containerIP(container); err == nil && ip != "" {
			resp, err := client.Get("http://" + ip + ":" + portStr(port) + "/")
			if err == nil {
				resp.Body.Close()
				return true
			}
		}
		if state, _ := e.runDocker(10*time.Second, "inspect", "-f", "{{.State.Status}}", container); state == "exited" {
			return false
		}
		time.Sleep(healthInterval)
	}
	return false
}

// portOf reads the health-check port from the container's shipd.port label.
func (e *Engine) portOf(container string) int {
	out, _ := e.runDocker(10*time.Second, "inspect", "-f", "{{index .Config.Labels \"shipd.port\"}}", container)
	n := 0
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}

func (e *Engine) containerIP(container string) (string, error) {
	out, err := e.runDocker(10*time.Second,
		"inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container)
	ips := strings.Fields(out)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("no ip: %v", err)
	}
	return ips[0], nil
}

// RunStop removes the app container (state kept for later start).
func (e *Engine) RunStop(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	app := e.st.Get(key)
	if app == nil {
		return
	}
	e.removeAppContainers(key, "")
	e.st.Update(key, func(a *App) {
		a.DesiredUp = false
		a.Status = StatusStopped
	})
	log.Printf("shipd: %s stopped", app.Subdomain)
}

// RunStart re-creates the container from the last built image, re-applying
// shipd.json env and routing labels (same set as deploy).
func (e *Engine) RunStart(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	app := e.st.Get(key)
	if app == nil {
		return
	}
	if app.Image == "" {
		e.st.Update(key, func(a *App) { a.LastError = "never deployed; nothing to start" })
		return
	}
	// re-read the repo's shipd.json so env/labels survive stop->start
	src := filepath.Join(e.cfg.BuildsDir(), slug(app.Repo)+"-"+hashKey(key))
	deck := e.readDeck(src)
	port := app.Port
	if port == 0 {
		port = deck.Port
	}
	if port == 0 {
		port = exposedPort(filepath.Join(src, orDefault(deck.Dockerfile, "Dockerfile")))
	}
	if port == 0 {
		e.st.Update(key, func(a *App) { a.LastError = "unknown port; redeploy instead" })
		return
	}
	if err := e.runContainer(app, app.Image, port, deck.Env); err != nil {
		e.st.Update(key, func(a *App) { a.LastError = "start failed: " + err.Error() })
		log.Printf("shipd: start %s failed: %v", app.Subdomain, err)
		return
	}
	status := StatusRunning
	lastErr := ""
	if !e.waitHealthy(app.containerName(), 15*time.Second) {
		log.Printf("shipd: %s started but did not answer probe within 15s (check logs)", app.Subdomain)
	}
	e.st.Update(key, func(a *App) {
		a.DesiredUp = true
		a.Status = status
		a.Port = port
		a.Container = app.containerName()
		a.LastError = lastErr
	})
	log.Printf("shipd: %s started", app.Subdomain)
}

// runContainer starts the stable-named container for an app with the full
// routing + env + port label set (used by deploy promotion and start).
// mergeEnv layers deploy-time env (app.Env, from `shipd deploy --env`) over
// repo-declared env (shipd.json). Deploy-time values win.
func mergeEnv(repoEnv []string, appEnv map[string]string) []string {
	if len(appEnv) == 0 {
		return repoEnv
	}
	ordered := append([]string{}, repoEnv...)
	idx := map[string]int{}
	for i, kv := range ordered {
		if k, _, ok := strings.Cut(kv, "="); ok {
			idx[k] = i
		}
	}
	keys := make([]string, 0, len(appEnv))
	for k := range appEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if i, ok := idx[k]; ok {
			ordered[i] = k + "=" + appEnv[k]
			continue
		}
		ordered = append(ordered, k+"="+appEnv[k])
	}
	return ordered
}

func (e *Engine) runContainer(app *App, image string, port int, env []string) error {
	// per-app persistent storage: host dir -> /data, created if missing.
	// It must be writable by the user the image actually runs as, or the
	// volume is decoration: a container running as uid 10007 cannot write to
	// a dir owned by the host user.
	dataDir := e.cfg.AppDataDir(app.Key())
	if err := os.MkdirAll(dataDir, 0o775); err != nil {
		return fmt.Errorf("create app data dir: %w", err)
	}
	// shipd runs unprivileged (and the unit sets RestrictSUIDSGID), so it
	// cannot chown the dir to the image's user, nor set the setgid bit.
	// Group-writability + --group-add is what makes /data usable: whoever the
	// image runs as joins shipd's group and can write. Files the container
	// creates keep the container's own uid, so host-side management is via the
	// directory (shipd owns it) rather than individual file ownership.
	if err := os.Chmod(dataDir, 0o775); err != nil {
		log.Printf("shipd: chmod app data dir: %v", err)
	}
	dataGID := strconv.Itoa(os.Getgid())

	env = mergeEnv(env, app.Env)
	runArgs := []string{
		"run", "-d", "--name", app.containerName(),
		"--network", e.cfg.Network,
		"--volume", dataDir + ":/data",
		"--group-add", dataGID,
		"--restart", "unless-stopped",
		"--label", "shipd.app=" + app.Key(),
		"--label", "shipd.subdomain=" + app.Subdomain,
		"--label", "shipd.port=" + fmt.Sprint(port),
		"--label", "traefik.enable=true",
		"--label", "traefik.docker.network=" + e.cfg.Network,
		"--label", "traefik.http.routers.app-" + app.Subdomain + ".rule=Host(`" + app.Domain + "`)",
		"--label", "traefik.http.routers.app-" + app.Subdomain + ".entrypoints=web",
		"--label", "traefik.http.services.app-" + app.Subdomain + ".loadbalancer.server.port=" + fmt.Sprint(port),
	}
	for _, env := range env {
		runArgs = append(runArgs, "--env", env)
	}
	runArgs = append(runArgs, image)
	if out, err := e.runDocker(2*time.Minute, runArgs...); err != nil {
		return fmt.Errorf("%v: %s", err, tail(out, 200))
	}
	return nil
}

// RunDelete removes everything for an app and drops it from state.
func (e *Engine) RunDelete(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	app := e.st.Get(key)
	if app == nil {
		return
	}
	// label filter catches containers under old subdomain names too
	e.removeAppContainers(key, "")
	_, _ = e.runDocker(time.Minute, "rm", "-f", app.containerName())
	e.st.mu.Lock()
	delete(e.st.Apps, key)
	e.st.mu.Unlock()
	e.st.Save()
	log.Printf("shipd: %s deleted", app.Subdomain)
}

// exposedPort parses the first EXPOSE from the given Dockerfile; 0 if none.
func exposedPort(dockerfilePath string) int {
	f, err := os.Open(dockerfilePath)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "EXPOSE ") {
			fields := strings.Fields(upper)
			if len(fields) >= 2 {
				var n int
				fmt.Sscanf(fields[1], "%d", &n)
				return n
			}
		}
	}
	return 0
}

// ---- small helpers -----------------------------------------------------

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func portStr(n int) string { return fmt.Sprint(n) }
