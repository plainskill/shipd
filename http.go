package main

import (
	"encoding/json"
	"net/http"
)

// guard enforces the shared API token via Basic auth (any username).
func (e *Engine) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || pass != e.cfg.APIToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="shipd"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
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
		defer st.mu.RUnlock()
		out := make([]App, 0, len(st.Apps))
		for _, a := range st.Apps {
			out = append(out, *a)
		}
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

func (e *Engine) handleLogs(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("subdomain")
	app := e.BySubdomain(sub)
	if app == nil {
		httpError(w, http.StatusNotFound, "no such app")
		return
	}
	lines := r.URL.Query().Get("lines")
	if lines == "" {
		lines = "200"
	}
	out, err := e.runDocker(logTimeout, "logs", "--tail", lines, app.containerName())
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

const dashHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>shipd</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { background:#000; color:#fff; font: 14px/1.6 "Fira Code", ui-monospace, monospace; margin:0; padding:48px 24px; }
  .col { max-width: 720px; }
  h1 { font-size: 20px; font-weight: 600; margin: 0 0 4px; }
  .muted { color: #717174; }
  .label { font-size: 11px; text-transform: uppercase; letter-spacing: .08em; color:#717174; margin: 28px 0 8px; }
  .app { border: 1px solid #1a1a1a; padding: 14px 16px; margin-bottom: 10px; }
  .row { display: flex; gap: 16px; align-items: baseline; flex-wrap: wrap; }
  a { color: #fff; text-decoration: underline; text-underline-offset: 3px; text-decoration-color: #3f3f43; }
  a:hover { text-decoration-color: #717174; }
  .status { font-size: 11px; padding: 1px 8px; border: 1px solid #1a1a1a; }
  .st-running  { color: #7ee787; border-color: #1f4a2a; }
  .st-building, .st-queued { color: #d2a8ff; border-color: #3b2a58; }
  .st-failed   { color: #ff7b72; border-color: #5a2320; }
  .st-stopped  { color: #717174; }
  .acts { margin-top: 8px; }
  .acts button, .acts a { font: inherit; font-size: 12px; background: none; color:#717174;
      border: 1px solid #1a1a1a; padding: 2px 10px; cursor: pointer; text-decoration: none; }
  .acts button:hover, .acts a:hover { color: #fff; border-color: #3f3f43; }
  input { font: inherit; background: #000; color: #fff; border: 1px solid #1a1a1a; padding: 6px 10px; width: 100%; }
  input:focus { outline: none; border-color: #3f3f43; }
  form { border: 1px solid #1a1a1a; padding: 14px 16px; }
  form .field { margin-bottom: 10px; }
  form button { font: inherit; font-size: 13px; background: #fff; color: #000; border: 0; padding: 6px 18px; cursor: pointer; }
  form button:hover { background: #d0d0d0; }
  .msg { font-size: 12px; margin-top: 10px; white-space: pre-wrap; }
  .err { color: #ff7b72; font-size: 12px; }
</style>
</head>
<body>
<div class="col">
  <h1>shipd</h1>
  <div class="muted">git in, containers out. single user.</div>

  <div class="label">deploy</div>
  <form id="deploy">
    <div class="field"><input id="repo" placeholder="https://git.plainskill.net/plainskill/shipd-example.git" required></div>
    <div class="field"><input id="branch" placeholder="branch (default: main)"></div>
    <div class="field"><input id="subdomain" placeholder="subdomain — app goes live at &lt;name&gt;.apps.plainskill.net" required></div>
    <button type="submit">deploy</button>
    <div class="msg muted" id="deploymsg"></div>
  </form>

  <div class="label">apps</div>
  <div id="apps" class="muted">loading...</div>
</div>
<script>
function esc(s) { return (s || "").replace(/[&<>"]/g, function(c) {
  return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
});}
function shortRepo(u) { return u.replace(/^https?:\/\//, "").replace(/^git@/, "").replace(/\.git$/, ""); }

function load() {
  fetch("/api/apps").then(function(r) {
    if (r.status === 401) { location.reload(); return null; }
    return r.json();
  }).then(function(apps) {
    if (!apps) return;
    var el = document.getElementById("apps");
    if (!apps.length) { el.textContent = "no apps yet — deploy one above"; return; }
    var html = "";
    for (var i = 0; i < apps.length; i++) {
      var a = apps[i];
      html += '<div class="app">'
        + '<div class="row">'
        + '<a href="https://' + esc(a.domain) + '/">' + esc(a.subdomain) + '</a>'
        + '<span class="muted">' + esc(shortRepo(a.repo)) + '@' + esc(a.branch) + (a.git_sha ? '@' + esc(a.git_sha) : '') + '</span>'
        + '<span class="status st-' + esc(a.status) + '">' + esc(a.status) + '</span>'
        + '</div>'
        + (a.last_error ? '<div class="err">' + esc(a.last_error) + '</div>' : '')
        + '<div class="muted" style="font-size:12px">last deploy: ' + esc(a.last_deploy ? new Date(a.last_deploy).toLocaleString() : "never") + '</div>'
        + '<div class="acts">'
        + '<a href="/api/apps/' + esc(a.subdomain) + '/logs" target="_blank">logs</a> '
        + '<button onclick="act(\'' + esc(a.subdomain) + '\',\'redeploy\')">redeploy</button> '
        + (a.desired_up
            ? '<button onclick="act(\'' + esc(a.subdomain) + '\',\'stop\')">stop</button> '
            : '<button onclick="act(\'' + esc(a.subdomain) + '\',\'start\')">start</button> ')
        + '<button onclick="act(\'' + esc(a.subdomain) + '\',\'delete\')">delete</button>'
        + '</div></div>';
    }
    el.innerHTML = html;
  });
}

function act(sub, action) {
  fetch("/api/apps/" + sub + "/" + action, { method: "POST" })
    .then(function(r) { if (r.status === 401) location.reload(); });
  setTimeout(load, 400);
}

document.getElementById("deploy").addEventListener("submit", function(ev) {
  ev.preventDefault();
  var msg = document.getElementById("deploymsg");
  msg.textContent = "queueing...";
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
      msg.textContent = "queued — status below updates as it builds";
      document.getElementById("repo").value = "";
      document.getElementById("branch").value = "";
      document.getElementById("subdomain").value = "";
      setTimeout(load, 500);
    } else {
      msg.textContent = "";
      msg.className = "msg err";
      msg.textContent = "error: " + (res.body.error || "unknown");
      setTimeout(function() { msg.className = "msg muted"; msg.textContent = ""; }, 6000);
    }
  });
});

load();
setInterval(load, 5000);
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
