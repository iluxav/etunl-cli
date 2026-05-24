---
name: etunl
description: Operate etunl — a reverse proxy and network tunnel that exposes local services through a public server, routing HTTP by subdomain and TCP via per-route ports. Use whenever the user asks to add, remove, list, restart, or diagnose tunnel routes; bootstrap an etunl server or client; connect to a remote TCP service through the tunnel; manage the admin UI or rotate tokens; investigate why a route is 404/502; or inspect etunl state on disk (config, systemd unit, dashboard). Also use when writing a config that etunl should hot-reload.
files:
  - scripts/check_prereqs.sh
  - scripts/diagnose.sh
  - references/commands.md
  - references/config.md
  - references/auth.md
  - references/architecture.md
  - references/troubleshooting.md
  - assets/example-config.yaml
---

# etunl

A Go reverse proxy + WebSocket tunnel that publishes local services (HTTP and
TCP) under subdomains of a public server. Each machine that runs the client
opens one persistent WebSocket to the server and multiplexes all of its routes
over it. Source: https://github.com/iluxav/ntunl

The CLI binary is `etunl`. This file is the entry point — for deep dives, jump
into the reference files below.

## When to read what

| File | When |
| --- | --- |
| This file | Always — orientation, safe vs. mutating commands, hard rules |
| `references/commands.md` | Need the full flag set for a specific subcommand |
| `references/config.md` | Touching `~/.etunl/config.yaml` or `~/.etunl/server.yaml` |
| `references/auth.md` | Adding/removing auth on an HTTP route, or login won't work |
| `references/architecture.md` | Need to reason about what hits server vs. client vs. local proxy |
| `references/troubleshooting.md` | A route returns 404/502, TCP won't connect, dashboard 401, etc. |
| `scripts/check_prereqs.sh` | First touch on a host — confirm binary + config + service state |
| `scripts/diagnose.sh <route>` | Triage a sick route in one shot (config, server health, listener) |
| `assets/example-config.yaml` | Template for a client config with HTTP, TCP, and each auth form |

## Pre-flight: is this host ready?

Before touching anything, find out where you are. There are three host roles:

- **server** — runs `etunl server`, has `~/.etunl/server.yaml`, public IP
- **client** — runs `etunl client`, has `~/.etunl/config.yaml`, dashboard on :8080
- **third-party** — neither config; just running `etunl connect` to reach a route

Quick probe:

```bash
which etunl || true
etunl --version 2>/dev/null || true
ls ~/.etunl/ 2>/dev/null
```

If `etunl` is missing, point the user at the install one-liner — **do not
install it yourself without explicit user approval**:

```
curl -fsSL https://raw.githubusercontent.com/iluxav/ntunl/main/install.sh | sh
```

For a fuller pre-flight (config validity, systemd unit state, dashboard
reachable, tunnel `/health` reachable from the client), run
`scripts/check_prereqs.sh` and read its output.

## Read-only investigation (plain shell)

These never mutate state — safe to run without confirmation.

**On a client host:**

- `etunl list` — show configured routes (NAME / TYPE / TARGET / LOCAL_PORT)
- `cat ~/.etunl/config.yaml` — full client config (CONTAINS THE TUNNEL TOKEN — see secrets rule below)
- `curl -fsS http://localhost:8080/api/status` — dashboard status JSON (connection, routes)
- `curl -fsS http://localhost:8080/api/metrics` — per-route throughput counters
- `systemctl status etunl 2>/dev/null` — Linux service state
- `journalctl -u etunl --since '5 min ago' --no-pager` — recent client logs

**On a server host:**

- `curl -fsS http://localhost/health` — connected machines, routes, allocated TCP ports
- `curl -fsS -u <admin>:<pass> http://localhost/api/status` (with admin Host header) — full admin status
- `cat ~/.etunl/server.yaml` — server config (CONTAINS TUNNEL TOKEN + ADMIN PASSWORD + SESSION SECRET)
- `ss -ltnp | grep etunl` — what ports etunl is listening on

**On any host:**

- `etunl --help`, `etunl <subcommand> --help` — built-in usage

## Mutating operations

Every command in this section changes config, daemon state, or both. Make the
consequence **visible to the user before the command runs**:

- State the action and its effect in the message preceding the call.
- For destructive verbs (`remove`, password rotation, service stop/uninstall,
  rewriting a config), ask before doing it.

### Bootstrap (first time on a fresh host)

**Server:**

```
etunl init --mode server
# writes ~/.etunl/server.yaml with a generated token and prints it
etunl server
```

**Client:**

```
etunl init --mode client --server tunnel.example.com <token-from-server>
# writes ~/.etunl/config.yaml with the token + a pre-configured `admin` route
etunl client
```

After `init` on the client, `https://admin.<domain>` will route to the
client's built-in dashboard (`localhost:8080`) once both ends are connected.

**`etunl init` without `--mode` defaults to client.** If you intend a server,
say so explicitly.

### Manage routes (hot-reloaded — NO daemon restart needed)

The client watches its config file with fsnotify and re-syncs to the server on
any change. CLI verbs and dashboard edits both go through the file; manual
edits work too. **Do not restart `etunl client` after a route change** — it
masks bugs in the watcher and drops in-flight connections.

```
etunl add --name api --type http --target localhost:8080
etunl add --name redis --type tcp --target localhost:6379 --local-port 16379
etunl remove --name api
etunl list
```

For TCP routes, `--local-port` is the port the **same-machine** local proxy
listens on. The **server** port is allocated automatically from
`tcp_port_range` (hash of route name with linear scan on collision) — read it
back from `curl localhost/health` on the server.

### Ad-hoc TCP tunneling from a third machine

`etunl connect` opens a fresh WebSocket per incoming connection and forwards
to a named route. The route must already exist on a connected client.

```
etunl connect --name db1 --local-port 5432
# now: psql -h localhost -p 5432
```

If you omit `--server` and `--token`, they're loaded from the local
`~/.etunl/config.yaml` if present.

### Dashboard, admin UI, token rotation

The **client dashboard** (port 8080) lists and edits the client's own routes.
It's served plain on `localhost:8080` and tunneled out via the pre-configured
`admin` route as `https://admin.<domain>`.

The **server admin UI** runs on the `manage` subdomain by default
(`admin_subdomain` in `server.yaml`, but the default is literally `manage`).
First visit forces credential setup; after that, HTTP basic auth gates it.

- `POST /api/setup` (one-shot, open until credentials are set)
- `POST /api/rotate-token` rotates the tunnel token. **All connected clients
  will be kicked off** and must be reconfigured with the new token — `etunl
  init --mode client --server <addr> <new-token>` re-runs cleanly because
  init overwrites `~/.etunl/config.yaml`.

Do not rotate the token without warning the user that every client config has
to be updated.

### Running the daemon

`etunl client` and `etunl server` are long-running processes. **Never** start
them in the foreground from an agent shell — they don't return and the tool
timeout will kill them mid-handshake, often leaving zombies on bound ports
(80, 8080, 15000-15100).

- If `install.sh` was used on Linux, a systemd unit `etunl` exists. Use
  `sudo systemctl restart etunl` / `sudo systemctl status etunl` instead.
- On macOS or hosts without systemd, tell the user to run `etunl client` (or
  `etunl server`) in their own terminal. Do not spawn it yourself.
- The client also exposes `--dashboard ":port"` (empty string disables). The
  server has no equivalent flag — admin UI is always on.

### Removing things

There is no `etunl uninstall`. To fully remove from a host:

```
sudo systemctl stop etunl && sudo systemctl disable etunl   # if installed as service
sudo rm /etc/systemd/system/etunl.service
sudo rm /usr/local/bin/etunl
# config is at ~/.etunl/ — leave it unless the user asks; it contains the token
```

`rm ~/.etunl/` is destructive and the user may want the token preserved.
Confirm before wiping.

## Diagnostic workflow when a route is broken

Use `scripts/diagnose.sh <route-name>` for the one-shot version. Manually:

1. **Is the route in the client config?** `etunl list` on the client
2. **Did the server accept it?** `curl localhost/health` on the server — look
   for the route under `machines[].routes`. If missing, the client either
   isn't connected or the route name collides with another machine's route
   (server logs `route … rejected: already owned by …`).
3. **Is the target up locally?** `curl http://<target>` (HTTP) or
   `nc -vz <target-host> <target-port>` (TCP) from the client machine.
4. **For HTTP**: `curl -H "Host: <route>.<domain>" http://<server>/` —
   bypasses DNS. 404 = no route; 502 = client disconnected mid-request; 401 =
   auth misconfigured.
5. **For TCP**: `curl localhost/health` on the server returns `tcp_ports`;
   confirm the listener exists and `nc -vz <server> <port>` reaches it.
6. **Watch logs while reproducing**: `journalctl -u etunl -f` (Linux service)
   or the user's terminal where they ran `etunl client`. Lines worth
   recognising: `tunnel client "X" connected`, `routes updated for "X": N
   accepted / M sent`, `tunnel disconnected: ... (reconnecting in Ns)`.
7. Fix the root cause — see `references/troubleshooting.md` for common
   patterns — and re-check with `curl localhost/health` on the server.

## Things you must NOT do

- **Don't** run `etunl client` or `etunl server` in the foreground from an
  agent shell. They're daemons. Use systemd, or hand the command to the user.
- **Don't** restart the daemon to pick up a route change. The watcher
  hot-reloads on file save. If reloading visibly isn't happening, that's a
  bug worth reporting — not something to paper over with a restart.
- **Don't** print or paste `~/.etunl/config.yaml`, `~/.etunl/server.yaml`,
  or anything containing the `token`, `session_secret`, or `admin_password`
  into user-visible output. These are secrets. Refer to them by file path.
- **Don't** delete `~/.etunl/` to "reset" — the token lives there, and
  losing it requires re-issuing on the server side and reconfiguring every
  client. Always confirm before wiping.
- **Don't** edit `server.yaml` to remove `tcp_port_range` thinking it'll
  pick a default. Empty range means **TCP tunneling is silently disabled**
  (see `server.go:50`). Defaults are filled only when the file doesn't exist.
- **Don't** rotate the admin token via `/api/rotate-token` without telling
  the user that every connected client needs to be re-initialised with the
  new token.
- **Don't** add an HTTP route whose name collides with an existing route on
  another connected machine. The server keeps the first owner and silently
  drops the colliding registration (you'll see `route … rejected` in the
  server log, but `etunl list` on the new machine will keep showing it).
