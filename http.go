package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// gateOnly requires the Caddy-injected shared secret on the dashboard paths.
// Those endpoints are unauthenticated by design — the authgate at Caddy
// authenticates the human — so this header is what proves the request came
// through Caddy and not from a sibling container on shipd-net (app
// containers share that network and can reach the gateway IP directly).
func (e *Engine) gateOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Shipd-Gate")), []byte(e.cfg.GateToken())) != 1 {
			httpError(w, http.StatusForbidden, "direct access denied")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard enforces authentication: root token from config or a managed token.
// Uses constant-time comparison via State.CheckToken.
func (e *Engine) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="shipd"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		ok, tokID := e.st.CheckToken(e.cfg.APIToken, pass)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="shipd"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		e.st.TouchToken(tokID)
		next.ServeHTTP(w, r)
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func handleAppsList(st *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st.mu.RLock()
		out := make([]App, 0, len(st.Apps))
		for _, a := range st.Apps {
			out = append(out, *a)
		}
		st.mu.RUnlock()
		// stable order for the dashboard's 5s poll
		sort.Slice(out, func(i, j int) bool { return out[i].Subdomain < out[j].Subdomain })
		writeJSON(w, http.StatusOK, out)
	}
}

type deployRequest struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Subdomain string `json:"subdomain"`
}

func (e *Engine) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Repo = trimGitSuffix(req.Repo)
	if !validRepoURL(req.Repo) {
		httpError(w, http.StatusBadRequest, "repo must be an https:// or git@ git url ending in .git")
		return
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	if !validBranch(req.Branch) {
		httpError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	sub := strings.TrimSpace(req.Subdomain)
	if sub != "" {
		if err := validSubdomain(sub); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		if e.reserved(sub) {
			httpError(w, http.StatusBadRequest, "subdomain is reserved")
			return
		}
		// one subdomain -> one app: reject if another repo already owns it
		if owner := e.BySubdomain(sub); owner != nil && owner.Key() != appKey(req.Repo, req.Branch) {
			httpError(w, http.StatusConflict, "subdomain already used by "+owner.Repo+"@"+owner.Branch)
			return
		}
	} else {
		// no subdomain given: redeploy in place if this repo+branch already
		// has a deployment, otherwise mint a random one
		if existing := e.st.Get(appKey(req.Repo, req.Branch)); existing != nil && existing.Subdomain != "" {
			sub = existing.Subdomain
		} else {
			rand, err := e.randomSubdomain()
			if err != nil {
				httpError(w, http.StatusInternalServerError, "could not allocate subdomain")
				return
			}
			sub = rand
		}
	}

	app := e.Upsert(req.Repo, req.Branch, sub)
	go e.RunDeploy(app.Key())
	writeJSON(w, http.StatusAccepted, map[string]any{"app": app, "message": "deployment queued"})
}

// handleDeleteByIdentity deletes the deployment for a repo+branch, which is
// the app's identity — this is what `shipd delete` uses.
func (e *Engine) handleDeleteByIdentity(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Repo = trimGitSuffix(req.Repo)
	if !validRepoURL(req.Repo) {
		httpError(w, http.StatusBadRequest, "repo must be an https:// or git@ git url")
		return
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	if !validBranch(req.Branch) {
		httpError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	key := appKey(req.Repo, req.Branch)
	app := e.st.Get(key)
	if app == nil {
		httpError(w, http.StatusNotFound, "no deployment for "+req.Repo+"@"+req.Branch)
		return
	}
	sub := app.Subdomain
	go e.RunDelete(key)
	writeJSON(w, http.StatusAccepted, map[string]string{"deleted": sub, "message": "deletion queued"})
}

func (e *Engine) handleAction(act action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sub := r.PathValue("subdomain")
		app := e.BySubdomain(sub)
		if app == nil {
			httpError(w, http.StatusNotFound, "no such app")
			return
		}
		switch act {
		case actionStop:
			go e.RunStop(app.Key())
		case actionStart:
			go e.RunStart(app.Key())
		case actionRedeploy:
			go e.RunDeploy(app.Key())
		case actionDelete:
			go e.RunDelete(app.Key())
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"app": app, "message": "action queued"})
	}
}

// ---- token management endpoints ---------------------------------------

type createTokenRequest struct {
	Name string `json:"name"`
}

// handleWhoami reports which token authenticated the request, so the CLI can
// confirm a login and show a human-readable identity.
func (e *Engine) handleWhoami(w http.ResponseWriter, r *http.Request) {
	_, pass, ok := r.BasicAuth()
	if !ok {
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	valid, tokID := e.st.CheckToken(e.cfg.APIToken, pass)
	if !valid {
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	name := "root"
	if tokID != "root" {
		e.st.mu.RLock()
		if t := e.st.Tokens[tokID]; t != nil {
			name = t.Name
		}
		e.st.mu.RUnlock()
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "id": tokID})
}

func (e *Engine) handleTokensList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, e.st.ListTokens())
}

func (e *Engine) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		httpError(w, http.StatusBadRequest, "name required (max 64 chars)")
		return
	}
	rec, plain, err := e.st.CreateToken(req.Name)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint failed")
		return
	}
	log.Printf("shipd: token created id=%s name=%q", rec.ID, rec.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":   rec.ID,
		"name": rec.Name,
		// shown exactly once
		"token":   plain,
		"created": rec.CreatedAt,
	})
}

func (e *Engine) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !e.st.RevokeToken(id) {
		httpError(w, http.StatusNotFound, "no such token")
		return
	}
	log.Printf("shipd: token revoked id=%s", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (e *Engine) handleLogs(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("subdomain")
	app := e.BySubdomain(sub)
	if app == nil {
		httpError(w, http.StatusNotFound, "no such app")
		return
	}
	lines := r.URL.Query().Get("lines")
	n, err := strconv.Atoi(lines)
	if err != nil || n < 1 {
		n = 200
	}
	if n > 5000 {
		n = 5000
	}
	out, err := e.runDocker(logTimeout, "logs", "--tail", strconv.Itoa(n), app.containerName())
	if err != nil && out == "" {
		httpError(w, http.StatusNotFound, "no container logs: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// handleCheck is the Caddy on_demand_tls ask endpoint. Token-protected via
// the derived ask token so third parties cannot probe domain provisioning.
func (e *Engine) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("t") != e.cfg.AskToken() {
		httpError(w, http.StatusUnauthorized, "bad token")
		return
	}
	if e.DomainAllowed(r.URL.Query().Get("domain")) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}
	httpError(w, http.StatusForbidden, "domain not provisioned")
}
