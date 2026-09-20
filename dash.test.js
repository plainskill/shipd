// Executes the dashboard's real embedded <script> against a stubbed DOM +
// fetch, and asserts the rendered markup. Catches syntax errors and rendering
// regressions without a browser or a session.
const fs = require("fs");
const re = /<script>([\s\S]*?)<\/script>/;

const html = fs.readFileSync(process.argv[2] || "dashboard.html", "utf8");
const code = html.match(re)[1];

function el(id) {
  return {
    id,
    _html: "",
    get innerHTML() { return this._html; },
    set innerHTML(v) { this._html = v; },
    textContent: "",
    value: "",
    listeners: {},
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
    getAttribute() { return null; },
    setAttribute() {},
    reset() {},
    closest() { return null; },
  };
}
const els = {};
const get = (id) => (els[id] = els[id] || el(id));

global.document = {
  getElementById: get,
  createElement: () => el("created"),
  querySelector: () => null,
  querySelectorAll: () => [],
  body: el("body"),
};
global.window = { location: { origin: "https://apps.example.net", href: "https://apps.example.net/" } };
global.location = global.window.location;
global.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
global.confirm = () => false;
global.alert = () => {};
global.setInterval = () => 0;
global.encodeURIComponent = encodeURIComponent;

const APPS = [{
  repo: "https://gt.plainskill.net/plainskill/demo.git", branch: "main",
  subdomain: "hello", domain: "hello.apps.plainskill.net", status: "running",
  git_sha: "abc1234", desired_up: true,
}];
// a genuinely hostile token name: apostrophe, double quote, angle brackets
const TOKENS = [{
  id: "87b06b5c", name: `sil'ver"frame<x>`, created_at: "2026-09-20T17:40:36Z",
}];

const fetchCalls = [];
global.fetch = async (path, opts) => {
  fetchCalls.push((opts && opts.method) || "GET");
  const body = String(path).includes("tokens") && !String(path).includes("revoke") ? TOKENS : APPS;
  return { status: 200, ok: true, json: async () => body, text: async () => JSON.stringify(body) };
};

// run the shipped script
new Function(code)();

setTimeout(() => {
  const apps = get("apps").innerHTML;
  const toks = get("tokens").innerHTML;
  const fails = [];
  const check = (cond, msg) => { if (!cond) fails.push(msg); };

  check(apps.includes("hello.apps.plainskill.net"), "apps panel did not render the app");
  check(apps.includes('data-act="redeploy"'), "apps panel missing redeploy button");
  check(apps.includes('data-act="delete"'), "apps panel missing delete button");
  check(!apps.includes("onclick="), "apps panel still uses inline onclick");
  check((apps.match(/>logs<\/a>/g) || []).length === 1, "logs link is duplicated in the apps panel");
  check(toks.includes("data-revoke=\"87b06b5c\""), "tokens panel missing revoke button with id");
  check(!toks.includes("onclick="), "tokens panel still uses inline onclick");
  check(!toks.includes("<x>"), "token name was not escaped in the markup");
  check(toks.includes("&#39;") && toks.includes("&quot;"), "token name was not properly entity-escaped");
  check(apps.trim() !== "loading..." && apps.trim() !== "", "apps panel still shows loading state");
  check(toks.trim() !== "loading..." && toks.trim() !== "", "tokens panel still shows loading state");

  console.log("fetch calls:", fetchCalls.join(","));
  console.log("apps panel:", apps.trim().slice(0, 120).replace(/\s+/g, " ") + " ...");
  console.log("tokens panel:", toks.trim().slice(0, 120).replace(/\s+/g, " ") + " ...");
  if (fails.length) {
    console.log("FAILURES:");
    fails.forEach((f) => console.log("  - " + f));
    process.exit(1);
  }
  console.log("DASHBOARD RENDER OK");
}, 50);
