# etunl CLI reference

Complete subcommand and flag reference. `etunl --help` and
`etunl <subcommand> --help` give the same info live.

## `etunl init [token]`

Create a config file (client by default; server with `--mode server`). If no
token is given, a fresh 32-byte hex token is generated and printed.

| Flag | Default | Description |
|---|---|---|
| `--mode <client\|server>` | `client` | Which config to write |
| `--server <addr>` | `tunnel.yourdomain.com` | (client mode) tunnel server address |
| `-c, --config <path>` | `~/.etunl/config.yaml` or `~/.etunl/server.yaml` | Config path |

### Client init

Writes `~/.etunl/config.yaml` with:

- `server: <addr>`
- `token: <given-or-generated>`
- `machine_name: <hostname>`
- One pre-configured route: `admin → localhost:8080` (the dashboard)

### Server init

Writes `~/.etunl/server.yaml` with:

- `listen_http: ":80"`
- `tcp_port_range: "15000-15100"`
- `token: <given-or-generated>`

Re-running `etunl init` **overwrites** the existing config file at that path
with no prompt. The auto-bootstrap path in `LoadServerConfig` will fill in
`session_secret` and `admin_subdomain: manage` on the next `etunl server`
start, so you don't need to set those by hand.

## `etunl server`

Run the tunnel server. Foreground, never returns until killed. Use
systemd in production.

| Flag | Default | Description |
|---|---|---|
| `-c, --config <path>` | `~/.etunl/server.yaml` | Server config path |

Listens on `listen_http` (HTTP + WebSocket upgrade at `/tunnel`, admin UI on
the `admin_subdomain` host, plain `/health` endpoint, all other host headers
→ HTTP proxy to the route's owning client). Also opens TCP listeners for any
TCP routes that connected clients claim, on ports allocated from
`tcp_port_range`.

## `etunl client`

Run the tunnel client. Foreground, never returns until killed. Use systemd
in production.

| Flag | Default | Description |
|---|---|---|
| `-c, --config <path>` | `~/.etunl/config.yaml` | Client config path |
| `--dashboard <addr>` | `:8080` | Dashboard listen address (empty string disables) |

On start:

1. Loads config + starts an fsnotify watcher on the config file
2. Starts the local proxy on `:80` (needs root or CAP_NET_BIND on Linux)
3. Starts a local TCP listener on `local_port` for each TCP route
4. Starts the dashboard (if `--dashboard` non-empty)
5. Dials `ws://<server>/tunnel` and re-syncs routes on every reconnect

Reconnect backoff is exponential, capped at 30 seconds.

## `etunl connect`

Open a local TCP listener and tunnel every accepted connection through the
server to a named TCP route on a connected client. Each incoming connection
opens its own WebSocket — no persistent registration.

| Flag | Default | Description |
|---|---|---|
| `--server <addr>` | from `~/.etunl/config.yaml` if present | Tunnel server address |
| `--token <token>` | from `~/.etunl/config.yaml` if present | Auth token |
| `--name <route>` | (required) | Route name on the target client |
| `--local-port <int>` | (required) | Port to listen on locally |
| `-c, --config <path>` | `~/.etunl/config.yaml` | Defaults source |

Useful for connecting a DB client on machine C to a database exposed via the
tunnel from machine B, without machine C needing its own `etunl client`
registration.

## `etunl add`

Add a route to the local client config and trigger hot-reload via the watcher.

| Flag | Default | Description |
|---|---|---|
| `--name <name>` | (required) | Route name (becomes the subdomain) |
| `--type <http\|tcp>` | `http` | Route type |
| `--target <addr>` | (required) | Local target (e.g. `localhost:3000`) |
| `--local-port <int>` | 0 (none) | Same-machine local listen port (TCP routes) |
| `-c, --config <path>` | `~/.etunl/config.yaml` | Config path |

Errors if a route with the same name already exists locally. **Does not
detect** cross-machine route name collisions — the server will silently
reject the duplicate at sync time.

## `etunl remove`

Remove a route from the local client config and trigger hot-reload.

| Flag | Default | Description |
|---|---|---|
| `--name <name>` | (required) | Route to remove |
| `-c, --config <path>` | `~/.etunl/config.yaml` | Config path |

## `etunl list`

Print the configured routes from the local client config in a fixed-width
table (NAME / TYPE / TARGET / LOCAL_PORT). Local view only — does not
contact the server; for the server's view use `curl <server>/health`.

| Flag | Default | Description |
|---|---|---|
| `-c, --config <path>` | `~/.etunl/config.yaml` | Config path |

## HTTP endpoints (server)

| Path | Auth | Purpose |
|---|---|---|
| `/tunnel` | `Authorization: Bearer <token>` + `X-Machine-Name: <name>` | WebSocket endpoint for clients |
| `/health` | none | JSON: connected machines, routes, allocated TCP ports |
| `<admin>./*` | basic auth (after `/api/setup` is called once) | Admin UI |

The `<admin>` subdomain defaults to `manage` (configurable via
`admin_subdomain` in `server.yaml`).

## HTTP endpoints (client dashboard)

Reachable on `localhost:8080` and tunneled out as `admin.<domain>`.

| Path | Purpose |
|---|---|
| `/` | Dashboard HTML |
| `/api/status` | Connection state and routes (JSON) |
| `/api/metrics` | Per-route byte/request counters |
| `/api/routes` (POST/DELETE) | Add or remove a route |
