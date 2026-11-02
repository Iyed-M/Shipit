# Shipit

Self-hosted deployment platform. Push to GitHub and your app is built, tested and live on its own HTTPS address.

Early development. The dashboard isn't built yet, so projects are managed through the API.

## Run it

You need a server with Docker, ports 80 and 443 open, and DNS records for `ship.example.com` and `*.ship.example.com`.

```sh
cp .env.example .env   # then fill it in
docker compose up -d --build
```

Create a project. Shipit adds the webhook to the repo, and the app appears at `https://<project>.ship.example.com` after the next push to `main`:

```sh
curl https://ship.example.com/api/projects \
  -H "Authorization: Bearer $SHIPIT_ADMIN_TOKEN" \
  -d '{"repo_url": "https://github.com/you/app"}'
```

The repo needs a `Dockerfile` at its root, and the app must listen on `$PORT`. An optional `shipit.toml` sets a test command, a health check path and resource limits:

```toml
[test]
command = "npm test"

[health]
path = "/healthz"

[resources]
memory = "512m"
cpus = 0.5
```

Tests run in the built image without the project's env vars. Limits default to the values above.

## How a deploy stays safe

Each push builds an image and starts it next to the version already running. The new version needs 3 health checks in a row within a minute: `GET` on `health.path` returning 2xx, or a TCP connect if no path is set. Only then does traffic move, so a version that fails leaves the old one serving, and its output is saved in the deploy log.

A **release** is an image plus a saved copy of its env vars, health check and limits. Env changes and rollbacks reuse the image and take the same path as a push, without rebuilding. A rollback also restores that release's env vars as the project's current ones.

If an app exits, Docker restarts it and Shipit records the crash. After 3 crashes within 2 minutes of going live, Shipit rolls back to the previous release. An automatic rollback doesn't trigger another one, so a release that crashes after being restored stays up for you to fix.

One build runs at a time, and if several commits are waiting for a project, only the newest is built. Env changes and rollbacks don't wait for builds. Each project keeps its last 3 images.

## API

All routes need the `Authorization: Bearer` header.

| Route | Purpose |
|---|---|
| `POST /api/projects` | Create a project: `repo_url`, optional `name` and `branch` |
| `GET /api/projects` | List projects |
| `GET /api/projects/{name}` | Show a project |
| `GET /api/projects/{name}/deploys` | List recent deploys |
| `GET /api/deploys/{id}` | Show a deploy and its stages |
| `GET /api/deploys/{id}/logs` | Read a deploy's output |
| `GET /api/projects/{name}/releases` | List releases that have been live |
| `GET /api/projects/{name}/crashes` | List recent crashes |
| `POST /api/projects/{name}/rollback` | Roll back: optional `release_id`, otherwise the previous release |
| `GET /api/projects/{name}/env` | List env var names |
| `PATCH /api/projects/{name}/env` | Change env vars: a string sets one, `null` removes it |

## Known limits

- Shipit needs the Docker socket, which is effectively root access on the server.
- Builds run on the host's Docker without a sandbox, so only deploy code you trust.
- It runs on one node, so there's no redundancy if the server goes down.
- Env vars are encrypted in Shipit's database, but anyone with Docker access can read them from a running container.
- Docker doesn't restart a container stopped with `docker kill`, so that isn't handled as a crash.

## License

MIT
