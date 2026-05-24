# etunl config reference

Two YAML files, both at `~/.etunl/` with mode `0600`. The client config is
fsnotify-watched and hot-reloaded; the server config is read at startup and
only re-saved when the admin UI mutates it (setup, token rotation).

## Client config — `~/.etunl/config.yaml`

```yaml
server: tunnel.example.com    # tunnel server host (no scheme, no path)
token: "<hex token>"           # must match server.token
machine_name: homelab1         # falls back to os.Hostname() if empty
routes:
  - name: app                  # subdomain key, must be unique across ALL connected machines
    type: http                 # "http" or "tcp"
    target: localhost:3030     # where the client forwards to
  - name: db1
    type: tcp
    target: localhost:5432
    local_port: 15432          # same-machine local TCP proxy listens here
```

| Field | Required | Notes |
|---|---|---|
| `server` | yes | Host (plus port if non-80, e.g. `etunl.com:8443`); WS URL is `ws://<server>/tunnel` |
| `token` | yes | Long random secret, must equal `server.yaml`'s `token` |
| `machine_name` | recommended | Used for route ownership tracking on the server. If empty, `os.Hostname()` is used. **Empty hostname + empty `machine_name` is a startup error.** |
| `routes[]` | yes (may be empty) | See below |

### Route fields

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Subdomain key. The HTTP URL becomes `https://<name>.<server-base-domain>/`. |
| `type` | yes | `http` or `tcp` |
| `target` | yes | `host:port` the client forwards to (typically `localhost:<port>`) |
| `local_port` | TCP only, optional | Port the **same-machine** local proxy listens on. Omit if you only want the route reachable via the tunnel. |
| `auth` | HTTP only, optional | One of three forms — see `references/auth.md` |

### Hot-reload semantics

- The client uses `fsnotify` to watch `~/.etunl/config.yaml`. Save the file
  and the new routes are synced to the server immediately.
- The watcher swallows transient parse errors and logs them — it does **not**
  crash on bad YAML, but it does **not** apply the new state either. Tail
  `journalctl -u etunl` (Linux service) to confirm reload happened.
- `etunl add` / `etunl remove` write through the same file, so they trigger
  the same reload path. Dashboard mutations go via the dashboard API which
  also writes the file.

### Connection lifecycle

When the watcher fires, the client calls `Mux().SendRouteSync(newRoutes)` on
the existing WebSocket — **no reconnect, no disruption to in-flight
streams**. If the underlying WebSocket has already dropped, the new routes
will be picked up on the next reconnect.

## Server config — `~/.etunl/server.yaml`

```yaml
listen_http: ":80"                     # HTTP + WebSocket + admin UI listen address
tcp_port_range: "15000-15100"          # allocator pool for TCP routes
token: "<hex token>"                   # must equal every client's token
session_secret: "<hex secret>"         # signs session cookies (admin UI + route session auth)
admin_subdomain: "manage"              # subdomain that serves the admin UI
admin_user: ""                         # set on first /api/setup call
admin_password: ""                     # set on first /api/setup call (stored as plain text — yes, really)
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `listen_http` | yes | `:80` | TCP listener for all HTTP-side traffic |
| `tcp_port_range` | recommended | `15000-15100` | **Empty disables TCP tunneling silently** (logged as WARNING at startup but easy to miss). Format `start-end`, must be within 1-65535, start < end. |
| `token` | yes | generated on first load | Bearer token clients send on the `/tunnel` WebSocket upgrade |
| `session_secret` | auto-filled | generated on first load | HMAC key for cookie-based session auth. Rotating it invalidates every active session. |
| `admin_subdomain` | no | `manage` | Host header trigger for the admin UI |
| `admin_user` / `admin_password` | bootstrapped via UI | empty | Initial setup is via the open `/api/setup` POST. Once set, the endpoint locks. **Password is stored in plaintext** — protect file perms. |

### Bootstrap behaviour (`LoadServerConfig`)

When `etunl server` starts and the config file is missing, the server
generates `token`, `session_secret`, `admin_subdomain`, and the default
listen/range — writes them, then continues. When the file exists but has
empty `token` or `session_secret`, the server fills them in and re-saves.
This is the **only** path that defaults `tcp_port_range`; if you write a
config file with `tcp_port_range: ""` explicitly, TCP is disabled.

## File paths and permissions

```
~/.etunl/
├── config.yaml      mode 0600  (client config, contains token)
└── server.yaml      mode 0600  (server config, contains token + admin pwd + session secret)
```

Both `SaveClientConfig` and `SaveServerConfig` chmod the file to 0600 and the
parent directory to 0700. If you see anything more permissive, treat it as a
red flag and ask the user before fixing (the user may have a custom umask
setup that lost on a copy).

## How config flows on the wire

The client sends a `RouteInfo` slice to the server every time the local
config changes:

```go
type RouteInfo struct {
    Name      string
    Type      string       // "http" | "tcp"
    LocalPort int          // not used by the server
    Auth      *RouteAuth   // bearer/header/users, all present
}
```

The `Target` field is intentionally **not** sent — only the owning client
knows where to forward to. The server just tracks (route name → owning
machine → WebSocket conn) and forwards bytes.

Auth secrets ARE sent to the server over the (typically TLS-terminating)
WebSocket — the server enforces auth for bearer/header/session forms on the
public-facing HTTP path. The client's local proxy enforces bearer/header
only (session is server-only because the cookie domain doesn't match a
local hostname).
