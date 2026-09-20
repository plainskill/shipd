package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if err := validSubdomain(req.Subdomain); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if e.reserved(req.Subdomain) {
		httpError(w, http.StatusBadRequest, "subdomain is reserved")
		return
	}
	// one subdomain -> one app: reject if another repo already owns it
	if owner := e.BySubdomain(req.Subdomain); owner != nil && owner.Key() != appKey(req.Repo, req.Branch) {
		httpError(w, http.StatusConflict, "subdomain already used by "+owner.Repo+"@"+owner.Branch)
		return
	}

	app := e.Upsert(req.Repo, req.Branch, req.Subdomain)
	go e.RunDeploy(app.Key())
	writeJSON(w, http.StatusAccepted, map[string]any{"app": app, "message": "deployment queued"})
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

// ---- dashboard (design language: plainskill.net Dash) ------------------
//
// Anatomy: left-aligned max-w-xl column, #000 bg, white fg, Fira Code,
// muted #717174, border #1a1a1a, radius 0, label/title/body/badge,
// quiet text links (no colored buttons).

const dashHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>shipd</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { background:#000; color:#fff; font: 13px/1.6 "Fira Code", ui-monospace, SFMono-Regular, Menlo, monospace;
         margin:0; padding:56px 24px; }
  .col { max-width: 36rem; }
  h1 { font-size: 18px; font-weight: 600; margin: 0 0 6px; letter-spacing: -0.01em; }
  .body { color: #717174; }
  .label { font-size: 10px; text-transform: uppercase; letter-spacing: .1em; color:#717174; margin: 32px 0 10px; }
  .panel { border: 1px solid #1a1a1a; padding: 16px; }
  .panel + .panel { margin-top: 10px; }
  .row { display: flex; gap: 12px; align-items: baseline; flex-wrap: wrap; }
  .grow { flex: 1 1 auto; }
  a { color: #fff; text-decoration: underline; text-underline-offset: 3px; text-decoration-color: #2a2a2e; }
  a:hover { text-decoration-color: #717174; }
  .badge { font-size: 10px; letter-spacing: .05em; text-transform: uppercase;
           padding: 1px 8px; border: 1px solid #1a1a1a; color: #717174; white-space: nowrap; }
  .b-running  { color: #7ee787; border-color: #1f4a2a; }
  .b-building, .b-queued { color: #d2a8ff; border-color: #3b2a58; }
  .b-failed   { color: #ff7b72; border-color: #5a2320; }
  .b-revoked  { color: #717174; text-decoration: line-through; }
  .meta { color: #717174; font-size: 11px; }
  .quiet { background: none; border: 0; padding: 0; font: inherit; font-size: 12px; color: #717174;
           cursor: pointer; text-decoration: underline; text-underline-offset: 3px; text-decoration-color: #2a2a2e; }
  .quiet:hover { color: #fff; text-decoration-color: #717174; }
  .quiet.warn:hover { color: #ff7b72; text-decoration-color: #5a2320; }
  input { font: inherit; font-size: 13px; background: #000; color: #fff; border: 1px solid #1a1a1a;
          padding: 7px 10px; width: 100%; }
  input:focus { outline: none; border-color: #3f3f43; }
  input::placeholder { color: #4a4a4e; }
  .field { margin-bottom: 10px; }
  .actions { margin-top: 10px; }
  .msg { font-size: 12px; margin-top: 10px; white-space: pre-wrap; }
  .err { color: #ff7b72; }
  .ok { color: #7ee787; }
  pre.token { border: 1px solid #1f4a2a; color: #7ee787; padding: 10px 12px; overflow-x: auto;
              font-size: 12px; margin: 10px 0 0; }
  .divider { border: 0; border-top: 1px solid #1a1a1a; margin: 0; }
  footer { margin-top: 40px; }
</style>
</head>
<body>
<div class="col">
  <h1>shipd</h1>
  <div class="body">git in, containers out. deploys land at &lt;name&gt;.apps.plainskill.net with automatic TLS.</div>

  <div class="label">deploy</div>
  <div class="panel">
    <form id="deploy">
      <div class="field"><input id="repo" placeholder="git url — https://host/org/repo.git" autocomplete="off" required></div>
      <div class="field"><input id="branch" placeholder="branch (default: main)" autocomplete="off"></div>
      <div class="field"><input id="subdomain" placeholder="subdomain — hello" autocomplete="off" required></div>
      <div class="actions"><button class="quiet" type="submit">deploy &rarr;</button></div>
      <div class="msg body" id="deploymsg"></div>
    </form>
  </div>

  <div class="label">apps</div>
  <div id="apps" class="body">loading...</div>

  <div class="label">api tokens</div>
  <div class="panel">
    <form id="tokform">
      <div class="field"><input id="tokname" placeholder="token name — e.g. laptop, ci, phone" autocomplete="off" required></div>
      <div class="actions"><button class="quiet" type="submit">create token &rarr;</button></div>
      <div class="msg body" id="tokmsg"></div>
      <div id="newtoken"></div>
    </form>
  </div>
  <div id="tokens" class="body" style="margin-top:10px">loading...</div>

  <footer>
    <hr class="divider">
    <div class="meta">single-user deploy plane &middot;
      <a href="/healthz">health</a> &middot;
      <a href="https://git.plainskill.net/plainskill/shipd">source</a>
    </div>
  </footer>
</div>
<script>
function esc(s) { return (s || "").replace(/[&<>"]/g, function(c) {
  return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
});}
function shortRepo(u) { return u.replace(/^https?:\/\//, "").replace(/^git@/, "").replace(/\.git$/, ""); }
function fmtDate(iso) { try { return new Date(iso).toLocaleString(); } catch (e) { return iso; } }
function badge(status) { return '<span class="badge b-' + esc(status) + '">' + esc(status) + '</span>'; }

function loadApps() {
  fetch("/api/apps").then(function(r) {
    if (r.status === 401) { location.reload(); return null; }
    return r.json();
  }).then(function(apps) {
    if (!apps) return;
    var el = document.getElementById("apps");
    if (!apps.length) { el.textContent = "no apps yet — deploy one above."; return; }
    var html = "";
    for (var i = 0; i < apps.length; i++) {
      var a = apps[i];
      html += '<div class="panel">'
        + '<div class="row">'
        + '<span class="grow"><a href="https://' + esc(a.domain) + '/">' + esc(a.subdomain) + '</a></span>'
        + badge(a.status)
        + '</div>'
        + '<div class="meta">' + esc(shortRepo(a.repo)) + ' @ ' + esc(a.branch)
        + (a.git_sha ? ' @ ' + esc(a.git_sha) : '') + '</div>'
        + (a.last_error ? '<div class="meta err">' + esc(a.last_error) + '</div>' : '')
        + '<div class="meta">last deploy: ' + esc(a.last_deploy ? fmtDate(a.last_deploy) : "never") + '</div>'
        + '<div class="actions">'
        + '<a href="/api/apps/' + esc(a.subdomain) + '/logs">logs</a> &nbsp; '
        + '<button class="quiet" onclick="act(\'' + esc(a.subdomain) + '\',\'redeploy\')">redeploy</button> &nbsp; '
        + (a.desired_up
            ? '<button class="quiet" onclick="act(\'' + esc(a.subdomain) + '\',\'stop\')">stop</button> &nbsp; '
            : '<button class="quiet" onclick="act(\'' + esc(a.subdomain) + '\',\'start\')">start</button> &nbsp; ')
        + '<button class="quiet warn" onclick="del(\'' + esc(a.subdomain) + '\')">delete</button>'
        + '</div></div>';
    }
    el.innerHTML = html;
  });
}

function act(sub, action) {
  fetch("/api/apps/" + sub + "/" + action, { method: "POST" })
    .then(function(r) { if (r.status === 401) location.reload(); });
  setTimeout(loadApps, 400);
}

function del(sub) {
  if (!confirm("delete " + sub + "? container and state are removed.")) return;
  fetch("/api/apps/" + sub + "/delete", { method: "POST" })
    .then(function(r) { if (r.status === 401) location.reload(); });
  setTimeout(loadApps, 500);
}

function loadTokens() {
  fetch("/api/tokens").then(function(r) {
    if (r.status === 401) { location.reload(); return null; }
    return r.json();
  }).then(function(toks) {
    if (!toks) return;
    var el = document.getElementById("tokens");
    if (!toks.length) { el.textContent = "no managed tokens. the root token is in /etc/shipd/config.json."; return; }
    var html = "";
    for (var i = 0; i < toks.length; i++) {
      var t = toks[i];
      var status = t.revoked_at ? "revoked" : "active";
      html += '<div class="panel">'
        + '<div class="row">'
        + '<span class="grow">' + esc(t.name) + '</span>'
        + badge(status)
        + '</div>'
        + '<div class="meta">created ' + esc(fmtDate(t.created_at))
        + (t.last_used_at ? ' &middot; last used ' + esc(fmtDate(t.last_used_at)) : ' &middot; never used')
        + '</div>'
        + (!t.revoked_at
            ? '<div class="actions"><button class="quiet warn" onclick="revoke(\'' + esc(t.id) + '\',\'' + esc(t.name) + '\')">revoke</button></div>'
            : '')
        + '</div>';
    }
    el.innerHTML = html;
  });
}

function revoke(id, name) {
  if (!confirm("revoke token '" + name + "'? anything using it stops working.")) return;
  fetch("/api/tokens/" + id + "/revoke", { method: "POST" })
    .then(function(r) { if (r.status === 401) location.reload(); });
  setTimeout(loadTokens, 400);
}

document.getElementById("deploy").addEventListener("submit", function(ev) {
  ev.preventDefault();
  var msg = document.getElementById("deploymsg");
  msg.textContent = "queueing...";
  msg.className = "msg body";
  fetch("/api/deploy", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      repo: document.getElementById("repo").value.trim(),
      branch: document.getElementById("branch").value.trim(),
      subdomain: document.getElementById("subdomain").value.trim()
    })
  }).then(function(r) {
    if (r.status === 401) { location.reload(); return null; }
    return r.json().then(function(b) { return { ok: r.ok, body: b }; });
  }).then(function(res) {
    if (!res) return;
    if (res.ok) {
      msg.textContent = "queued — status updates below as it builds.";
      msg.className = "msg ok";
      document.getElementById("repo").value = "";
      document.getElementById("branch").value = "";
      document.getElementById("subdomain").value = "";
      setTimeout(loadApps, 500);
    } else {
      msg.textContent = "error: " + (res.body.error || "unknown");
      msg.className = "msg err";
    }
    setTimeout(function() { msg.textContent = ""; }, 8000);
  });
});

document.getElementById("tokform").addEventListener("submit", function(ev) {
  ev.preventDefault();
  var msg = document.getElementById("tokmsg");
  var box = document.getElementById("newtoken");
  msg.textContent = "";
  box.innerHTML = "";
  fetch("/api/tokens", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: document.getElementById("tokname").value.trim() })
  }).then(function(r) {
    if (r.status === 401) { location.reload(); return null; }
    return r.json().then(function(b) { return { ok: r.ok, body: b }; });
  }).then(function(res) {
    if (!res) return;
    if (res.ok) {
      box.innerHTML = '<div class="msg ok">token created. copy it now — it is shown once:</div>'
        + '<pre class="token">' + esc(res.body.token) + '</pre>'
        + '<div class="msg body">use it: curl -u shipd:' + esc(res.body.token) + ' https://shipd.plainskill.net/api/apps</div>';
      document.getElementById("tokname").value = "";
      loadTokens();
    } else {
      msg.textContent = "error: " + (res.body.error || "unknown");
      msg.className = "msg err";
    }
  });
});

loadApps();
loadTokens();
setInterval(loadApps, 5000);
</script>
</body>
</html>`

func (e *Engine) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashHTML))
}
