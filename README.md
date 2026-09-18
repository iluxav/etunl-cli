# etunl

A reverse proxy and network tunnel that exposes local services through a public server. Route HTTP services by subdomain and TCP services (databases, etc.) through a single tunnel connection. Supports multiple client machines per server, optional per-route auth, a built-in client dashboard, and a server admin panel with live metrics.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/iluxav/etunl-cli/main/install.sh | sh
```

Or pin a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/iluxav/etunl-cli/main/install.sh | VERSION=v0.1.0 sh
```

The installer downloads the `etunl` binary for your OS/arch (linux/darwin, amd64/arm64/armv7), verifies its checksum, and installs it to `/usr/local/bin/etunl`. It then offers to:

- run interactive setup (`etunl init`) in server or client mode
- on Linux, install and start an `etunl` systemd service

Re-running the installer upgrades the binary and restarts an existing `etunl` service.

### Build from source

```bash
git clone https://github.com/iluxav/etunl-cli.git
cd etunl-cli
go build -o etunl ./cmd/etunl
```

## Architecture

```
Internet → Cloudflare (TLS) → Public server (etunl server)
                                    │
                         ┌──────────┴──────────┐
                  WebSocket tunnel      WebSocket tunnel
                         │                     │
              Machine A (etunl client)   Machine B (etunl client)
                         │
              ┌──────────┼──────────┐
            :3000      :3030      :5432
            server      app        db
                         │
                  :8080 dashboard
```

- **HTTP**: routed by subdomain via the `Host` header (`app.yourdomain.com` → `localhost:3030` on the owning machine). WebSockets are supported.
- **TCP**: each TCP route gets its own port on the server, allocated from `tcp_port_range`.
- **Multiple machines**: each client connects with a `machine_name`. Route names are global — the first machine to claim a name owns it.
- **Admin panel**: `manage.yourdomain.com` shows connected machines, routes, and traffic metrics.
- **Local access**: the client also runs a local proxy, so routes work on your LAN without going through the tunnel.

## Quick Start

```bash
# 1. On the server (public machine)
etunl init --mode server      # prints a token
etunl server

# 2. On each client machine — use the token from the server
etunl init --mode client --server tunnel.yourdomain.com <token>
etunl client
```

The client `init` pre-configures an `admin` route pointing to the built-in dashboard, so once connected you can open `https://admin.yourdomain.com` to manage routes from your browser.

To add routes via CLI instead:

```bash
etunl add --name app --type http --target localhost:3030
```

## Setup

### 1. Server (public machine, e.g. DigitalOcean)

```bash
etunl init --mode server
etunl server
```

This creates `~/.etunl/server.yaml`. If the file doesn't exist when `etunl server` starts, it is created with defaults. Missing `token` / `session_secret` values are generated and saved automatically.

```yaml
listen_http: ":80"
tcp_port_range: "15000-15100"   # ports handed out to TCP routes
token: "your-secret-token"      # shared with clients
admin_subdomain: "manage"       # admin panel at manage.yourdomain.com
session_secret: "..."           # signs login cookies (auto-generated)
admin_user: ""                  # set via the admin panel on first visit
admin_password: ""
```

Open the TCP port range in your firewall if you use TCP routes.

### 2. Client (each local machine)

```bash
etunl init --mode client --server tunnel.yourdomain.com <token>
etunl client
```

This creates `~/.etunl/config.yaml` with the token, your hostname as `machine_name`, and an `admin` route for the dashboard. A full example:

```yaml
server: tunnel.yourdomain.com
token: "your-secret-token"
machine_name: homelab1

routes:
  - name: admin
    type: http
    target: localhost:8080

  - name: app
    type: http
    target: localhost:3030

  - name: db1
    type: tcp
    target: localhost:5432
    local_port: 15432   # port for local/LAN access
```

See [`config.example.yaml`](config.example.yaml) for more.

### 3. DNS (Cloudflare)

Add DNS records pointing to your server:

```
yourdomain.com    →  A  →  <server IP>   (Proxied)
*.yourdomain.com  →  A  →  <server IP>   (Proxied)
```

Set SSL/TLS mode to **Flexible** (Cloudflare handles TLS, server listens on HTTP).

Cloudflare's proxy only forwards HTTP(S), so TCP routes must be reached by the server's IP (or a DNS-only record) on the allocated port.

### 4. Local and LAN access

The client runs a local proxy on port 80 (subdomain routing for HTTP routes) and on each TCP route's `local_port`. It listens on all interfaces, so other machines on your network can use it too.

On the client machine, add to `/etc/hosts`:

```
127.0.0.1  app.local.env
127.0.0.1  server.local.env
```

On other machines on the LAN, point the same names at the client machine's IP instead:

```
192.168.1.50  app.local.env
192.168.1.50  server.local.env
```

TCP routes are available at `<client IP>:<local_port>`.

## Route auth

HTTP routes can be protected. Pick **one** form per route:

```yaml
routes:
  # Bearer token: Authorization: Bearer <secret>
  - name: api
    type: http
    target: localhost:8080
    auth:
      bearer: "long-random-secret"

  # Custom header
  - name: webhook
    type: http
    target: localhost:9000
    auth:
      header: "X-API-Key"
      value: "long-random-secret"

  # Browser login (cookie session)
  - name: app
    type: http
    target: localhost:3030
    auth:
      users:
        - user: alice
          password: "change-me"
```

- Bearer and header auth are checked at the tunnel server and at the local proxy.
- Browser login is enforced at the tunnel server only. Unauthenticated visitors are redirected to `/___login___`; sign out with `/___logout___`.
- Auth is only supported on `http` routes.

## Client dashboard

The client includes a web dashboard for managing routes, on port 8080 by default.

- **Locally**: `http://localhost:8080`
- **Remotely**: `https://admin.yourdomain.com` (via the pre-configured `admin` route)

Login uses HTTP basic auth: any username, with the tunnel token as the password.

From the dashboard you can view tunnel status, and see, add, and remove routes. Changes are saved to the config file and hot-reloaded — no restart needed.

```bash
# Custom dashboard port
etunl client --dashboard :9090

# Disable dashboard
etunl client --dashboard ""
```

## Server admin panel

The server serves an admin panel at `https://<admin_subdomain>.yourdomain.com` (default `manage`). On first visit it asks you to create an admin username and password, which are saved to `server.yaml`.

It shows connected machines, their routes (including which auth scheme each uses), allocated TCP ports, and per-route traffic metrics.

## Usage

### Manage routes (CLI)

```bash
# Add an HTTP route
etunl add --name api --type http --target localhost:8080

# Add a TCP route
etunl add --name redis --type tcp --target localhost:6379 --local-port 16379

# List routes
etunl list

# Remove a route
etunl remove --name api
```

Routes are hot-reloaded — no restart needed.

### Remote TCP access

From any machine, tunnel a local port to a TCP route through the server:

```bash
etunl connect --name db1 --local-port 5432
```

`--server` and `--token` are read from `~/.etunl/config.yaml` if present, or can be passed as flags. Then connect your client (DBeaver, psql, etc.) to `localhost:5432`.

### Running as a service (Linux)

The installer can set up a systemd unit. To manage it:

```bash
sudo systemctl status etunl
sudo journalctl -u etunl -f
sudo systemctl restart etunl
```

### Health check

```bash
curl https://yourdomain.com/health
```

Returns status, connected machines and their routes, and allocated TCP ports.

## Commands

| Command | Description |
|---|---|
| `etunl init [token]` | Create a client (`--mode client`, default) or server (`--mode server`) config; generates a token if not provided |
| `etunl server` | Run the tunnel server on a public machine |
| `etunl client` | Run the tunnel client, local proxy, and dashboard |
| `etunl connect` | Tunnel a local port to a remote TCP route |
| `etunl add` | Add a route to the config |
| `etunl remove` | Remove a route from the config |
| `etunl list` | List configured routes |

All commands accept `-c/--config` to use a non-default config path. Use `etunl <command> --help` for details.
