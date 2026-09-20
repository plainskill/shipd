# shipd

Single-user PaaS in one Go binary. Git URL + branch + subdomain in, app live at
`<subdomain>.apps.plainskill.net` out. TLS is automatic (Caddy on-demand TLS,
gated by shipd's `/check` endpoint). No database — state is one JSON file.

## Why

Existing options (Coolify, Dokploy) assume they own the whole box: their own
proxy on 80/443, their own database, their own concept of "servers". This host
already runs Caddy as the edge. shipd is the ~800-line answer: a deploy plane
that lives *behind* an existing reverse proxy instead of replacing it.

## Deploy something

    curl -u :$TOKEN https://shipd.plainskill.net/api/deploy \
      -d '{"repo": "https://git.plainskill.net/plainskill/shipd-example.git",
           "branch": "main",
           "subdomain": "example"}'

App identity is (repo, branch). Re-deploying the same pair replaces it;
a new subdomain can be assigned anytime by deploying the same pair with a
different subdomain. Repos may be any git URL reachable from the host
(GitHub, Forgejo, etc.). Private repos work if the deploy host has a
deploy key / credential for the URL.

## shipd.json (optional, in the repo root)

    {
      "port": 8000,              // container listen port (else: first EXPOSE)
      "dockerfile": "Dockerfile",
      "context": ".",            // build context subdir
      "env": ["KEY=value", ...]  // runtime env vars
    }

## API (Basic auth, user ignored, password = API token)

| Method | Path                              | What                     |
|--------|-----------------------------------|--------------------------|
| GET    | /api/apps                         | list apps                |
| POST   | /api/deploy                       | deploy {repo,branch,subdomain} |
| POST   | /api/apps/{sub}/redeploy          | rebuild + swap           |
| POST   | /api/apps/{sub}/stop              | stop (keeps state)       |
| POST   | /api/apps/{sub}/start             | start from last image    |
| POST   | /api/apps/{sub}/delete            | remove container + state |
| GET    | /api/apps/{sub}/logs?lines=200    | container logs           |
| GET    | /healthz                          | liveness (no auth)       |
| GET    | /check?domain=...&t=TOKEN         | Caddy on-demand TLS gate |

## How a deploy works

1. `git clone --depth 1` (or fetch) into /data/shipd/builds/<slug>
2. `docker build` with shipd.json/Dockerfile defaults
3. push to localhost:5000 (local registry, best-effort)
4. run new container on `shipd-net` under a temp name with Traefik labels
5. HTTP health probe against the container; on failure: remove temp, old
   version keeps serving (automatic rollback)
6. swap: old container removed, temp promoted

## Layout on the host

- binary: /usr/local/bin/shipd
- config: /etc/shipd/config.json (0600 root)
- data:   /data/shipd/ (state.json, builds/, logs via journald)
- unit:   /etc/systemd/system/shipd.service
- listens on 127.0.0.1:8900 only; Caddy fronts it at shipd.plainskill.net

## Routing

Caddy owns 80/443 (as always). For `<sub>.apps.plainskill.net` Caddy uses
on-demand TLS with `ask` pointing at shipd `/check` (token in query). shipd
answers OK only for provisioned app domains. Traffic path:

    browser -> Caddy (TLS) -> traefik (apps-routing, :81 on caddy/shipd-net) -> app container

## Ops notes (pscA)

- shipd-net is a fixed-subnet bridge (172.16.0.0/24, gateway 172.16.0.1);
  shipd additionally listens on the gateway IP so containerized Caddy can
  reach the host process. UFW allows only shipd-net -> 8900.
- The Caddy `ask` URL embeds the derived ask token (sha256("shipd-ask:"+api_token)
  truncated to 32 hex) — rotate api_token in /etc/shipd/config.json AND the
  ask URL in the Caddyfile together.
- systemd unit hardening: ProtectSystem=strict, ProtectHome=tmpfs (shipd sets
  HOME=/data/shipd/home for docker buildx), no caps, docker via SupplementaryGroups.
