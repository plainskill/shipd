# shipd

**Vercel but cheaper.** Point it at a git repo, get a container at
`<subdomain>.apps.plainskill.net` with automatic TLS. One Go binary, no
database, single user.

```console
$ shipd deploy
no subdomain given — using 56131dc4488b
deploying https://forge.example.net/me/demo.git@main → https://56131dc4488b.<zone>
  building
  running
live at https://56131dc4488b.<zone>
```

(The install one-liner below and the `Ops` section at the end describe the
reference deployment; AGENTS.md holds the host-specific details.)

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
  "env": ["NODE_ENV=production", "PUBLIC_URL=https://myapp.apps.plainskill.net"]
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

Environment set with `shipd deploy --env KEY=value` (repeatable) is stored in
shipd's state file (0600) — **not** in the repo — and layered over anything
declared in `shipd.json`. `--env KEY=` removes a key. Values are applied to the
container and re-applied on `start`, and are never returned by the API: the
listing exposes key names only, so a leaked listing cannot hand over a secret.

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

> This describes the reference deployment's gate services. The concept is
> generic (put an authenticating proxy in front; shipd trusts it), the product
> names are not — see AGENTS.md.

shipd does **not** authenticate humans. That is deliberately delegated to the
infrastructure in front of it:

| layer | role |
|---|---|
| **authgate** (`plainskill-dash:4181 /verify`, a Caddy `forward_auth`) | **the actual authentication** — the browser session gate for the dashboard and every gated host |
| **abm** (`abm:4191 /check`, a Caddy `forward_auth`) | **anti-bot only** — proof-of-work challenge for suspicious clients. Not authentication. Applied to *all* hosts, including deployed apps |
| **API tokens** | **programmatic access only** — `curl`, CI, the CLI. `Authorization: Basic` with any username, token as the password |

Consequences worth knowing:

- `/api/*`, `/check` and `/healthz` bypass the authgate so token-authed
  tooling (and Caddy's own on-demand TLS ask) can reach them.
- The dashboard paths (`/dash/*`) are unauthenticated *at the shipd layer* —
  they rely on the authgate. Because app containers share the `shipd-net`
  network and can reach the gateway IP directly, those paths additionally
  require a `X-Shipd-Gate` header that only Caddy knows (derived from the API
  token). A deployed app cannot mint tokens or delete apps.
- Tokens are unscoped: any valid token is equivalent to root. Revoking deletes
  it outright.

## Deploying and updating shipd itself

The **local registry is the update channel**. shipd runs as a host systemd
service (it needs the docker CLI and a real `HOME` for git), and the registry
image is the artifact carrier:

```sh
make deploy            # build on pscA → push to localhost:5000 → update service
make deploy v1.3.0     # explicit version
make dist              # cross-compiled CLI binaries + SHA256SUMS
```

Anything that can push to the registry effectively executes code as root on the
host (the updater runs the pulled binary to read its version), unattended via
the weekly timer — keep registry write access short.

On the host, `/stack/compose/shipd/update.sh` pulls
`localhost:5000/atlas/shipd:latest`, extracts the binary from the image, swaps
it into `/usr/local/bin/shipd`, restarts, health-checks, and **rolls back
automatically** if the service does not come up. A weekly systemd timer
(`shipd-update.timer`, Mon 04:30) runs it unattended; run it by hand with
`sudo /stack/compose/shipd/update.sh`.

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
  -d '{"repo":"https://gt.plainskill.net/plainskill/shipd-example.git","branch":"main"}' \
  https://<your-shipd-host>/api/deploy
```

## Ops

| what | where |
|---|---|
| binary | `/usr/local/bin/shipd` |
| config | `/etc/shipd/config.json` (0640 root:plainskill — holds `api_token`) |
| state | `/data/shipd/state.json` (apps + tokens, 0600) |
| builds | `/data/shipd/builds/<slug>-<hash8>` |
| git HOME | `/data/shipd/home` (deploy keys go in `.ssh/` here) |
| unit | `/etc/systemd/system/shipd.service` (ProtectSystem=strict, no caps, docker via SupplementaryGroups) |
| listens | `127.0.0.1:8900` + `172.16.0.1:8900` (shipd-net gateway, for Caddy) |
| app network | `shipd-net` (172.16.0.0/24) — app containers live here. **Prerequisite:** create it with that explicit subnet (`docker network create --subnet 172.16.0.0/24 --gateway 172.16.0.1 shipd-net`); a default bridge gets a random subnet and the gateway in `listen_extra` / UFW / the ask URL will not exist. shipd warns at startup if an advertised listen address is missing |
| routing | Caddy (80/443, TLS) → Traefik `shipd-router:81` (label discovery) → app |
| firewall | UFW allows 8900 only from `172.16.0.0/24` |

Secrets derived from `api_token`: the ask token (`sha256("shipd-ask:"+token)`)
and the dashboard gate token (`sha256("shipd-gate:"+token)`). Rotating the API
token requires updating the Caddy `ask` URL and `X-Shipd-Gate` header to match.

## Repo layout

```
main.go config.go state.go engine.go http.go tokens.go dash.go   server (package main)
cli/main.go                                                      the shipd CLI
dashboard.html                                                   embedded dashboard UI
deploy/                                                          update.sh + systemd units
Dockerfile                                                       artifact image (registry channel)
install.sh                                                       one-line CLI installer
Makefile                                                         build / dist / image / deploy
```
