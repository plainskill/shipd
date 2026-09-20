# AGENTS.md — working with shipd

shipd is the homelab deploy plane ("Vercel but cheaper"): git repo in, container
at `<subdomain>.apps.plainskill.net` out. This file is for agents operating it.

## Fast paths

```sh
# deploy the repo you're in (keeps its subdomain, or mints a random one)
shipd deploy
shipd deploy excalidraw          # explicit subdomain
shipd delete                     # remove this repo's deployment
shipd apps                       # what's deployed
shipd logs [subdomain]           # container logs
shipd whoami                     # which server + token am I using
```

CLI config lives in `~/.config/shipd/config.json` (server + token, 0600).
`SHIPD_SERVER` / `SHIPD_TOKEN` override it. There is no default server — shipd
is host-agnostic; `shipd login` asks for it.

Raw API (tokens are for programmatic use):

```sh
curl -u shipd:$TOKEN https://<host>/api/apps
curl -u shipd:$TOKEN -H 'Content-Type: application/json' \
  -d '{"repo":"https://gt.plainskill.net/plainskill/x.git","branch":"main","subdomain":"x"}' \
  https://<host>/api/deploy
curl -u shipd:$TOKEN -H 'Content-Type: application/json' \
  -d '{"repo":"https://gt.plainskill.net/plainskill/x.git","branch":"main"}' \
  https://<host>/api/delete          # delete by identity
curl -u shipd:$TOKEN -X POST https://<host>/api/prune    # reclaim builds/images
```

## Auth model — do not confuse the two gates

- **authgate** = `forward_auth plainskill-dash:4181 { uri /verify }` — the real
  authentication for humans. Gates the dashboard and other hosts.
- **abm** = `forward_auth abm:4191 { uri /check }` — **anti-bot only, NOT
  auth.** It belongs on *every* host, including deployed apps and the port-80
  catch-all. Never describe abm as authentication, and never "fix" an auth
  problem by adding abm.
- **API tokens** = programmatic only (`curl`, CI, CLI). Unscoped: any valid
  token == root.

`/dash/*` and the dashboard page are unauthenticated at the shipd layer *by
design* (authgate does the human auth) but require the `X-Shipd-Gate` header
that Caddy injects — that is what stops a deployed app container on `shipd-net`
from reaching the control plane via `172.16.0.1:8900`.

## Deploy semantics

App identity is **(repo, branch)**; subdomain is an attribute.

- subdomain given → deploy there; 409 if another repo+branch owns it
- subdomain omitted + app exists → redeploy in place, same subdomain
- subdomain omitted + no app → random 12-hex subdomain, returned in response

Per-app env set with `--env K=V` lives in state (never the repo) and is masked
as `env_keys` in API responses. Apps get `<data>`→`/data` bind-mounted, so
redeploys keep SQLite state; data outlives `delete` and is never auto-removed.

Pipeline: clone → build → run temp **without routing labels** → HTTP probe by
container IP → promote (remove old, run stable with labels). A failed probe
rolls back and the old version keeps serving. Do not "simplify" this by giving
the temp container router labels — unvalidated code would take live traffic.

## Operating the server (pscA)

| thing | where |
|---|---|
| binary | `/usr/local/bin/shipd` |
| config | `/etc/shipd/config.json` (0640 root:plainskill) |
| state | `/data/shipd/state.json` (apps + tokens + per-app env) |
| app data | `/data/shipd/apps/<app-key-hash>` → `/data` in each container (mode 0775 + `--group-add 1000`, so a non-root image USER can write); `by-name/<sub>` symlinks. `shipd prune` never deletes these. Setgid is impossible: the unit sets `RestrictSUIDSGID=true` |
| builds | `/data/shipd/builds/` |
| git HOME | `/data/shipd/home` (deploy keys in `.ssh/` here) |
| unit | `systemctl {status,restart} shipd`, `journalctl -u shipd` |
| update | `sudo /stack/compose/shipd/update.sh` (registry → binary swap + rollback). **Trust:** it runs the pulled binary as root, weekly and unattended against `:latest` — registry push access ≈ root on pscA |
| app network | `shipd-net` must keep its fixed subnet 172.16.0.0/24 (gateway 172.16.0.1). Recreating it without `--subnet/--gateway` breaks `listen_extra`, the UFW rule and the ask URL; shipd logs a warning at startup if the address is missing |
| routing | `/stack/compose/shipd-routing/` (Traefik `shipd-router`) |
| caddy | `/stack/compose/caddy/Caddyfile` |

Deploy/update shipd itself from the repo checkout on pscB:

```sh
make deploy            # build on pscA, push to localhost:5000, update, verify
make dist              # release binaries + SHA256SUMS
```

## Pitfalls

- **Caddyfile edits**: it is a single-file bind mount. Edit **in place**
  (`open(path,'w')` after read / truncate) — `sed -i` creates a new inode and
  the container keeps serving the stale file. Verify with
  `docker exec caddy md5sum /etc/caddy/Caddyfile` before trusting a reload.
- **abm coverage**: `sudo python3 /stack/compose/caddy/audit-abm.py` lists every
  site block and flags missing abm. `abm.plainskill.net` is the one intentional
  exception (the verify portal must not gate itself).
- **Container-reachable control plane**: any app container can reach
  `172.16.0.1:8900`. Only `/check` and `/healthz` are safe there; `/dash/*`
  requires the gate header. Never add an unguarded route to the gateway
  listener.
- **Hostnames**: Forgejo is `gt.plainskill.net` (not `git.`). Apps live under
  `apps.plainskill.net`. The apex wildcard is `*.plainskill.net`.
- **Registry image has no shell** (scratch): use `docker create` + `docker cp`
  to extract the binary, not `docker run`.
- **`docker create` needs a CMD** — the Dockerfile carries `CMD ["/shipd"]`
  for exactly this reason.
- **git needs HOME**: git runs with `HOME=/data/shipd/home`; `ProtectHome=tmpfs`
  in the unit means the real home is unusable.
- **Deleting a subdomain's app** while a deploy is in flight is safe — the
  deploy re-checks state before promoting and discards the build.
