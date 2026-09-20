package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	logTimeout     = 30 * time.Second
	healthTimeout  = 45 * time.Second
	healthInterval = 2 * time.Second
	registryHost   = "localhost:5000"
	shipdNetwork   = "shipd-net"
)

// Engine executes deploys and docker/git operations against the host.
type Engine struct {
	cfg *Config
	st  *State

	// mu serializes deploy pipelines (one build at a time; single-user box).
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

// ---- engine operations ------------------------------------------------

func (e *Engine) runDocker(timeout time.Duration, args ...string) (string, error) {
	return runCmd(timeout, "docker", args...)
}

func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

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
	a.Image = fmt.Sprintf("%s/shipd/%s:%s", registryHost, slug(repo), slug(branch))
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
	out, err := e.runDocker(30*time.Second, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return fmt.Errorf("network ls: %v: %s", err, out)
	}
	for _, n := range strings.Fields(out) {
		if n == shipdNetwork {
			return nil
		}
	}
	if out, err := e.runDocker(time.Minute, "network", "create", shipdNetwork); err != nil {
		return fmt.Errorf("network create: %v: %s", err, out)
	}
	log.Printf("shipd: created network %s", shipdNetwork)
	return nil
}

// Reconcile aligns status with reality (e.g. after a host reboot).
func (e *Engine) Reconcile() {
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

// checkout clones (depth 1) or updates a repo working copy at dst.
func (e *Engine) checkout(repo, branch, dst string) (string, error) {
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		if out, err := runCmd(5*time.Minute, "git", "clone", "--depth", "1", "--branch", branch,
			"--single-branch", repo, dst); err != nil {
			return out, fmt.Errorf("clone %s@%s: %v: %s", repo, branch, err, tail(out, 200))
		}
	} else {
		if out, err := runCmd(time.Minute, "git", "-C", dst, "fetch", "--depth", "1", "origin", branch); err != nil {
			return out, fmt.Errorf("fetch: %v: %s", err, tail(out, 200))
		}
		if out, err := runCmd(time.Minute, "git", "-C", dst, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return out, fmt.Errorf("checkout: %v: %s", err, tail(out, 200))
		}
	}
	return runCmd(15*time.Second, "git", "-C", dst, "rev-parse", "HEAD")
}

// RunDeploy is the full pipeline: checkout -> build -> run -> health -> swap.
func (e *Engine) RunDeploy(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	app := e.st.Get(key)
	if app == nil {
		return
	}
	src := filepath.Join(e.cfg.BuildsDir(), slug(app.Repo)+"-"+slug(app.Branch))
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

	// 2. resolve port: shipd.json -> Dockerfile EXPOSE
	deck := e.readDeck(src)
	port := deck.Port
	if port == 0 {
		port = exposedPort(src)
	}
	if port == 0 {
		fail(fmt.Errorf("no listen port: add shipd.json {\"port\": N} or EXPOSE in Dockerfile"))
		return
	}

	// 3. build
	image := app.Image
	buildArgs := []string{
		"build",
		"-f", filepath.Join(src, orDefault(deck.Dockerfile, "Dockerfile")),
		"-t", image,
		"--network", "host",
		"--label", "shipd.app=" + key,
		"--label", "shipd.subdomain=" + app.Subdomain,
		src,
	}
	if out, err := e.runDocker(30*time.Minute, buildArgs...); err != nil {
		fail(fmt.Errorf("build: %v: %s", err, tail(out, 500)))
		return
	}

	// 4. push (best-effort; the local image is what actually runs)
	if out, err := e.runDocker(5*time.Minute, "push", image); err != nil {
		log.Printf("shipd: push %s failed (continuing with local image): %v: %s", image, err, tail(out, 200))
	}

	// 5. run new version under a temp name with routing labels
	temp := app.tempContainerName(sha)
	_, _ = e.runDocker(time.Minute, "rm", "-f", temp) // stale same-sha container
	runArgs := []string{
		"run", "-d", "--name", temp,
		"--network", shipdNetwork,
		"--restart", "unless-stopped",
		"--label", "shipd.app=" + key,
		"--label", "shipd.subdomain=" + app.Subdomain,
		"--label", "shipd.port=" + fmt.Sprint(port),
		"--label", "traefik.enable=true",
		"--label", "traefik.docker.network=" + shipdNetwork,
		"--label", "traefik.http.routers.app-" + app.Subdomain + ".rule=Host(`" + app.Domain + "`)",
		"--label", "traefik.http.routers.app-" + app.Subdomain + ".entrypoints=web",
		"--label", "traefik.http.services.app-" + app.Subdomain + ".loadbalancer.server.port=" + fmt.Sprint(port),
	}
	for _, env := range deck.Env {
		runArgs = append(runArgs, "--env", env)
	}
	runArgs = append(runArgs, image)
	if out, err := e.runDocker(2*time.Minute, runArgs...); err != nil {
		fail(fmt.Errorf("run: %v: %s", err, tail(out, 300)))
		return
	}

	// 6. health check; on failure remove temp — old version keeps serving
	if !e.waitHealthy(temp, healthTimeout) {
		state, _ := e.runDocker(10*time.Second, "inspect", "-f", "{{.State.Status}}", temp)
		logs, _ := e.runDocker(15*time.Second, "logs", "--tail", "20", temp)
		_, _ = e.runDocker(time.Minute, "rm", "-f", temp)
		fail(fmt.Errorf("health check failed (state=%s); rolled back. recent logs: %s",
			strings.TrimSpace(state), tail(strings.ReplaceAll(logs, "\n", " | "), 300)))
		return
	}

	// 7. swap: remove old stable container, promote temp
	stable := app.containerName()
	_, _ = e.runDocker(time.Minute, "rm", "-f", stable)
	if out, err := e.runDocker(time.Minute, "rename", temp, stable); err != nil {
		log.Printf("shipd: rename %s -> %s: %v: %s (continuing; routing is label-based)",
			temp, stable, err, out)
	}

	e.st.Update(key, func(a *App) {
		a.Status = StatusRunning
		a.Port = port
		a.Container = stable
		a.LastError = ""
		a.LastDeploy = time.Now().UTC()
	})
	log.Printf("shipd: %s deployed %s in %s", app.Subdomain, short(sha), time.Since(started).Round(time.Second))
}

// waitHealthy probes http://<ip>:<port>/ until it answers or timeout.
// The port comes from the container's shipd.port label.
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
	app := e.st.Get(key)
	if app == nil {
		return
	}
	_, _ = e.runDocker(time.Minute, "rm", "-f", app.containerName())
	e.st.Update(key, func(a *App) {
		a.DesiredUp = false
		a.Status = StatusStopped
	})
	log.Printf("shipd: %s stopped", app.Subdomain)
}

// RunStart re-creates the container from the last built image.
func (e *Engine) RunStart(key string) {
	app := e.st.Get(key)
	if app == nil {
		return
	}
	if app.Image == "" || app.Port == 0 {
		e.st.Update(key, func(a *App) { a.LastError = "never deployed or unknown port; redeploy instead" })
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.runContainer(app, app.Image); err != nil {
		e.st.Update(key, func(a *App) { a.LastError = "start failed: " + err.Error() })
		log.Printf("shipd: start %s failed: %v", app.Subdomain, err)
		return
	}
	e.st.Update(key, func(a *App) {
		a.DesiredUp = true
		a.Status = StatusRunning
		a.Container = app.containerName()
	})
	log.Printf("shipd: %s started", app.Subdomain)
}

// runContainer starts the stable-named container for an app with routing labels.
func (e *Engine) runContainer(app *App, image string) error {
	_, _ = e.runDocker(time.Minute, "rm", "-f", app.containerName())
	runArgs := []string{
		"run", "-d", "--name", app.containerName(),
		"--network", shipdNetwork,
		"--restart", "unless-stopped",
		"--label", "shipd.app=" + app.Key(),
		"--label", "shipd.subdomain=" + app.Subdomain,
		"--label", "traefik.enable=true",
		"--label", "traefik.docker.network=" + shipdNetwork,
		"--label", "traefik.http.routers.app-" + app.Subdomain + ".rule=Host(`" + app.Domain + "`)",
		"--label", "traefik.http.routers.app-" + app.Subdomain + ".entrypoints=web",
		"--label", "traefik.http.services.app-" + app.Subdomain + ".loadbalancer.server.port=" + fmt.Sprint(app.Port),
		image,
	}
	if out, err := e.runDocker(2*time.Minute, runArgs...); err != nil {
		return fmt.Errorf("%v: %s", err, tail(out, 200))
	}
	return nil
}

// RunDelete removes the container and drops the app from state.
func (e *Engine) RunDelete(key string) {
	app := e.st.Get(key)
	if app == nil {
		return
	}
	_, _ = e.runDocker(time.Minute, "rm", "-f", app.containerName())
	e.st.mu.Lock()
	delete(e.st.Apps, key)
	e.st.mu.Unlock()
	e.st.Save()
	log.Printf("shipd: %s deleted", app.Subdomain)
}

// exposedPort parses the first EXPOSE from the repo Dockerfile; 0 if none.
func exposedPort(src string) int {
	f, err := os.Open(filepath.Join(src, "Dockerfile"))
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
