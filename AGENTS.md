# AGENTS.md — operating shipd

Guidance for agents working **on** shipd (the code in this repo).

shipd is deployment-agnostic: nothing here assumes a particular host, domain, or
edge proxy. The running instance's specifics — host layout, gate services,
paths, the update pipeline — live with that deployment's ops files, not in this
repo.

## What shipd does

Point it at a git repo + branch and it builds and runs a container at
`<subdomain>.<domain>`, with TLS handled by whatever edge proxy sits in front.
App identity is **(repo, branch)**; the subdomain is an attribute.

## Deploy semantics (do not simplify these)

Pipeline: clone → build → run a temp container **without routing labels** →
HTTP-probe it by container IP → promote.

- A failed probe discards the temp container and the previous version keeps
  serving (rollback). Never give the temp container router labels — unvalidated
  code would take live traffic.
- Promotion re-checks state first: a delete or stop that lands mid-build wins,
  and the build is discarded.
- `removeAppContainers(key, keep)` resolves container IDs to names before
  comparing `keep` (`docker ps` returns IDs). It cleans up exited orphans too
  (`ps -aq`), which is what makes subdomain reassignment safe.

## Auth model

Three distinct things — do not conflate them:

| layer | role |
|---|---|
| **edge authentication** (an authenticating forward proxy in front) | authenticates humans. shipd trusts the edge for the dashboard |
| **anti-bot** (a bot-filtering forward proxy, if deployed) | **not** authentication. Applies to every host, including deployed apps |
| **API tokens** | programmatic access only (curl / CI / CLI). Unscoped: any valid token == root |

`/dash/*` and the dashboard page are unauthenticated at the shipd layer *by
design* — the edge authenticates. Because app containers share the app network
and can reach the gateway listener directly, those paths require the
`X-Shipd-Gate` header that only the edge knows (derived from the API token). Any
new route on the gateway listener must either be safe to expose to app
containers or be gated the same way.

## Configuration

`domain` and `api_token` are **required** — there is no default domain, so a
misconfigured deploy cannot silently publish into someone else's zone. `registry`
is optional: empty means local-only images and no push attempt. `network` /
`network_subnet` control the app network; pinning the subnet matters when a
proxy's upstream or a firewall rule depends on the gateway address (shipd warns
at startup if an advertised `listen_extra` address is missing locally).

Per-app environment lives in shipd state (never the repo) and is exposed as
`env_keys` only — values must never be returned by the API. Sources, in
increasing precedence: `shipd.json` `env` (public, read server-side), the
`.shipd.env` file the CLI reads from the working tree (`--env-path` overrides it;
`--env-unset K` removes a key), then `--env K=V`.

An empty value means an empty value, and `env_unset` removes keys — do not
conflate "unset" with "empty", a dotenv file uses `K=` for the latter.

## Testing

`make vet` runs `go vet`, the Go test suite, a JS syntax check of the embedded
dashboard script, and `dash.test.js` (which executes the real embedded script
against a stub DOM and asserts the rendered markup). Anything touching the
dashboard must keep that passing — a JS syntax error in the embedded script
disables the entire page silently.

## Pitfalls

- **Caddyfile-style config files**: a single-file bind mount edited with a
  rewrite (e.g. `sed -i`) changes the inode and the container keeps reading the
  old file. Edit in place, or restart the container.
- **`docker create` needs a CMD** — the artifact image carries `CMD ["/shipd"]`
  for exactly this reason. The image has no shell either: extract the binary
  with `docker create` + `docker cp`, never `docker run`.
- **shipd runs unprivileged** and cannot chown to another uid, nor set setgid
  when the unit sets `RestrictSUIDSGID`. Per-app `/data` is made usable with
  group ownership + `--group-add`, not chown.
- **git needs a HOME**: `ProtectHome=tmpfs` in a hardened unit means git must be
  invoked with an explicit writable `HOME` (shipd uses `<data_dir>/home`).
- **Deploys ship the remote, not the working tree** — the clone is what makes
  deploys reproducible. `shipd deploy` with no origin prints the remedy.
- **Non-HTTP apps** fail the probe by design; they need an HTTP shim.
- **Only `/data` persists** across deploys (keyed by app identity — branch
  included). State written elsewhere in the container is lost; a DB-backed app
  must point its data path at `/data`.
- **Edge discovery latency**: right after promotion the app container is healthy
  but the proxy may 404 until it reloads its routes. Set `edge_probe` so a deploy
  is only reported successful once the app answers through the edge.
