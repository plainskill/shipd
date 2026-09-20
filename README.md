# shipd

**Vercel but cheaper.** Point it at a git repo, get a container at
`<subdomain>.<your-domain>` with automatic TLS. One Go binary, no database,
single user.

```console
$ shipd deploy
no subdomain given — using 56131dc4488b
deploying https://forge.example.net/me/demo.git@main → https://56131dc4488b.<zone>
  building
  running
live at https://56131dc4488b.<zone>
```

(The install one-liner and `install.sh` point at this repo's own forge — they
are how this project ships its CLI, not a deployment assumption.)

## Install the CLI

```sh
curl -fsSL https://gt.plainskill.net/plainskill/shipd/raw/branch/main/install.sh | sh
```

Installs to `~/.local/bin/shipd` (override with `SHIPD_BIN_DIR`), verifies the
release checksum, supports linux/darwin on amd64/arm64.

Then connect it to a server — the CLI is host-agnostic, nothing is baked in:

```sh
shipd login                    # asks for the server url, then the token
shipd whoami                                     # which server, which token
```

Tokens are created in the dashboard (`https://<your-shipd-host>/` → *api
tokens*). `shipd login` verifies the token before storing it, and reads the
secret without echoing so it never lands in shell history.

## Using it

| command | what it does |
|---|---|
| `shipd deploy` | deploy the repo you're in |
| `shipd deploy excalidraw` | deploy it on the `excalidraw` subdomain |
| `shipd deploy --branch dev` | deploy another branch (a separate app) |
| `shipd deploy --env K=V` | set per-app environment (secrets — stays out of git) |
| `shipd delete` | remove this repo's deployment |
| `shipd delete excalidraw` | remove a named deployment |
| `shipd apps` | list deployments |
| `shipd logs [subdomain]` | container logs |
| `shipd prune` | reclaim builds + images for deleted apps |

Apps are identified by **(repo, branch)**. The subdomain is an attribute:

- **subdomain given** → deploy there (conflict = 409 if another repo owns it)
- **subdomain omitted, app exists** → redeploy in place, keeping its subdomain
- **subdomain omitted, no app** → a random subdomain is minted (12 hex chars)
  and returned in the response

Environment overrides: `SHIPD_SERVER`, `SHIPD_TOKEN`.

## How a deploy works

1. `git clone --depth 1` of the branch (git runs with `HOME=<data>/home`, so
   deploy keys placed there work for `git@`/private repos)
2. `docker build` — `shipd.json` in the repo root can override defaults
3. the new container starts **without routing labels** and is health-probed
   directly by IP
4. only if the probe passes: the old container is removed, the validated image
   is promoted under the stable name *with* routing labels
5. probe failure = the temp container is discarded and the previous version
   keeps serving (automatic rollback)

### `shipd.json` (optional, repo root)

```json
{
  "port": 3000,
  "dockerfile": "Dockerfile",
  "context": ".",
  "env": ["NODE_ENV=production", "PUBLIC_URL=https://myapp.example.net"]
}
```

`port` falls back to the first `EXPOSE` in the Dockerfile. `context` and
`dockerfile` are clamped inside the repo (a hostile `../..` is rejected, and
symlinks out of the repo are refused). `env` is applied to the running
container and re-applied on `start`.

### Persistent data

Every app gets a host directory bind-mounted at `/data`, so a redeploy does not
wipe state — this is what makes a SQLite app survive:

```
<data-dir>/apps/<app-key-hash>/          # the app's /data
<data-dir>/apps/by-name/<subdomain>      # symlink, for humans
```

The storage is keyed by app identity (repo+branch), so it follows the app even
if the subdomain changes. The directory is group-writable and the container is
started with `--group-add <shipd's gid>`, so a container running as a non-root
`USER` can still write to `/data` (shipd runs unprivileged and cannot chown). Deleting an app leaves its data on disk; `shipd
prune` reports orphaned data directories but never deletes them.

### Secrets

Environment for an app comes from three places, later overriding earlier:

1. **`shipd.json` `env`** — *public* values that belong in the repo.
2. **`.shipd.env`** (repo root) — a dotenv file, read from your working tree by
   `shipd deploy`. Silently skipped when absent, which is the normal case.
3. **`shipd deploy --env K=V`** — explicit, per-invocation.

```sh
shipd deploy                       # reads ./.shipd.env if it exists
shipd deploy --env-path prod.env   # use this file instead (warns if missing)
shipd deploy --env K=V             # override a single value from the file
shipd deploy --env-unset K         # remove a stored variable
```

`--env-path` replaces `.shipd.env` entirely rather than merging with it.

File format is dotenv: `KEY=VALUE` per line, `#` comments, blank lines ignored,
an optional `export ` prefix, and single/double quotes. Inline ` # comment` is
stripped from unquoted values. A malformed line is reported and skipped — one
typo never blocks a deploy. `KEY=` sets an empty value; use `--env-unset` to
remove a key.

Values are stored in shipd's state (0600) — **never in the repo** — applied to
the container, and re-applied on `start`. They persist, so a redeploy from the
dashboard (which has no access to your working tree) keeps them. The API exposes
`env_keys` only: values are never returned.

> **If your env file is tracked by git**, `shipd deploy` says so:
> `warn: .shipd.env is tracked by git — secrets will ship in the deploy`
> A tracked file is cloned server-side on every deploy and lives in the repo
> history. Prefer adding `.shipd.env` to `.gitignore`; `shipd.json` is for the
> public values that are meant to be committed.

### What does not fit

- **Non-HTTP apps**: the health probe expects an HTTP response on `/` before
  promoting. A worker, bot, or cron-style script will build and then fail the
  probe and roll back. Deploy it behind a tiny HTTP shim if you need it here.
- **Deploys ship the remote, not the working tree**: shipd clones server-side,
  so the code has to be pushed first. That is what makes a deploy reproducible
  (the same commit builds the same image). For a brand-new idea that means:
  create the repo on the forge, `git push -u origin main`, then `shipd deploy`.
- **No buildpacks**: bring a Dockerfile.

## Auth model — external gating

shipd does **not** authenticate humans. That is deliberately delegated to the
infrastructure in front of it:

| layer | role |
|---|---|
| **the authenticating edge** (a forward-auth proxy in front) | authenticates humans. shipd trusts it for the dashboard |
| **anti-bot filtering** (if deployed) | **not** authentication — a separate concern, and it belongs on *every* host including deployed apps |
| **API tokens** | **programmatic access only** — `curl`, CI, the CLI. Basic auth, any username, token as the password |

Consequences worth knowing:

- `/api/*`, `/check` and `/healthz` bypass the interactive gate so token-authed
  tooling (and the edge's on-demand TLS ask) can reach them.
- The dashboard paths (`/dash/*`, and the page itself) are unauthenticated *at
  the shipd layer* — the edge authenticates. Because app containers share the
  app network and can reach the gateway listener directly, those paths also
  require an `X-Shipd-Gate` header that only the edge knows (derived from the
  API token), so a deployed app cannot mint tokens or delete apps.
- Tokens are unscoped: any valid token is equivalent to root. Revoking deletes
  it outright.

## Running it

shipd is a host process (it shells out to `docker` and `git`), run under a
service manager:

```ini
[Unit]
Description=shipd — single-user deploy plane
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
# a dedicated user, in the docker group — not root
User=shipd
Group=shipd
SupplementaryGroups=docker
ExecStart=/usr/local/bin/shipd -config /etc/shipd/config.json
Restart=on-failure
RestartSec=5
# hardening: shipd needs write access to its data dir and config dir only
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=tmpfs
ReadWritePaths=/var/lib/shipd /etc/shipd
PrivateTmp=true
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
```

Config (`config.json`): `domain` and `api_token` are required; `registry`,
`network`, `network_subnet`, `source_url`, `edge_probe`, `reserved` and
`listen_extra` are optional (see `config.go` for the full set). `edge_probe`
(e.g. `"https://{domain}/"`) makes a deploy report success only once the app
answers *through* the edge, closing the window where the container is healthy
but the proxy has not yet discovered its router. Point `listen_extra` at the app
network's gateway if a containerized edge proxy has to reach it, and pin
`network_subnet` when the proxy's upstream or a firewall rule depends on the
gateway address.

**Edge proxy.** Give the deployment's proxy:
- an on-demand-TLS allowlist pointed at `GET /check?domain=<host>&t=<ask token>`
  (the ask token is derived from `api_token`; shipd logs the URL shape at startup)
- a reverse proxy to the shipd listener, with `X-Shipd-Gate: <gate token>`
  injected on the dashboard paths (also derived from `api_token`)
- label-based discovery (Traefik's docker provider, for example) so promoted
  containers are routed by the labels shipd sets

**Updating.** Build the artifact image (`make image`) and push it to your
registry, then have your update script pull it, extract `/shipd` from the image,
swap the binary, restart, health-check, and roll back on failure. The image is a
delivery vehicle, not the runtime — it is a `scratch` image carrying the binary
(hence `CMD ["/shipd"]`), because shipd needs the host's docker and git.

## API

Basic auth, any username, token as password. All JSON.

| method | path | notes |
|---|---|---|
| POST | `/api/deploy` | `{repo, branch, subdomain?}` → 202 |
| POST | `/api/delete` | `{repo, branch}` → deletes by identity |
| GET | `/api/apps` | list deployments |
| POST | `/api/apps/{sub}/{stop,start,redeploy,delete}` | act on one app |
| GET | `/api/apps/{sub}/logs?lines=200` | container logs (clamped 1–5000) |
| GET | `/api/whoami` | `{name, id}` for the presented token |
| GET/POST | `/api/tokens` | list / create (`{name}` → plaintext shown once) |
| POST | `/api/tokens/{id}/revoke` | delete a token |
| GET | `/healthz` | liveness + version (no auth) |
| GET | `/check?domain=&t=` | Caddy on-demand TLS gate |

```sh
curl -u shipd:$TOKEN -H 'Content-Type: application/json' \
  -d '{"repo":"https://forge.example.net/me/demo.git","branch":"main"}' \
  https://<your-shipd-host>/api/deploy
```

## Ops

| what | where |
|---|---|
| binary | wherever `ExecStart` points (e.g. `/usr/local/bin/shipd`) |
| config | `-config` path, mode 0640 owned by the service user — it holds `api_token` |
| state | `<data_dir>/state.json` (apps, tokens, per-app env; 0600) |
| builds | `<data_dir>/builds/<slug>-<hash>` |
| app data | `<data_dir>/apps/<app-key-hash>` → `/data` in each container, plus `by-name/<subdomain>` symlinks |
| git HOME | `<data_dir>/home` — deploy keys go in `.ssh/` there |
| listens | the `listen` address (loopback) + any `listen_extra` (edge proxy) |
| app network | the `network` value (default `shipd-net`); pin `network_subnet` if the gateway address is load-bearing. shipd warns at startup if a `listen_extra` address is missing locally |
| TLS/routing | the edge proxy's job, not shipd's |

Deployment-specific specifics (hostnames, gate services, firewall rules, the
update pipeline) belong with that deployment's ops files, not in this repo.

## Repo layout

```
main.go config.go state.go engine.go http.go tokens.go dash.go   server (package main)
netcheck.go                                                      listen-address sanity check
cli/main.go                                                      the shipd CLI
dashboard.html                                                   embedded dashboard UI
Dockerfile                                                       artifact image (registry channel)
install.sh                                                       one-line CLI installer
dash.test.js                                                     dashboard render test
Makefile                                                         build / test / dist / image
```
