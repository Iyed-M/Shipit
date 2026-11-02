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

The repo needs a `Dockerfile` at its root, and the app must listen on `$PORT`. An optional `shipit.toml` sets a test command and resource limits:

```toml
[test]
command = "npm test"

[resources]
memory = "512m"
cpus = 0.5
```

## API

All routes need the `Authorization: Bearer` header.

| Route | Purpose |
|---|---|
| `POST /api/projects` | Create a project: `repo_url`, optional `name` and `branch` |
| `GET /api/projects` | List projects |
| `GET /api/projects/{name}` | Show a project |
| `GET /api/projects/{name}/deploys` | List recent deploys |
| `GET /api/deploys/{id}` | Show a deploy and its stages |
| `GET /api/deploys/{id}/logs` | Read a deploy's build and test output |

## Known limits

- Shipit needs the Docker socket, which is effectively root access on the server.
- Builds run on the host's Docker without a sandbox, so only deploy code you trust.
- It runs on one node, so there's no redundancy if the server goes down.

## License

MIT
