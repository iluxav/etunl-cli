# etunl architecture

How a request actually flows. Useful when reasoning about where to look for
a bug — server side, client side, or local proxy.

## Topology

```
Internet ──(TLS termination, e.g. Cloudflare)──> etunl server (public IP, :80)
                                                       │
                                              WebSocket /tunnel
                                                       │
                                              ┌────────┴────────┐
                                              │  etunl client   │
                                              │  (local host)   │
                                              └────────┬────────┘
                                                       │
                       ┌───────────────────────────────┼───────────────────────────────┐
                       ▼                               ▼                               ▼
              localhost:3000                   localhost:3030                   localhost:5432
              (HTTP service)                   (HTTP service)                   (TCP service)

Plus a same-machine bypass:
       Browser on the client host ──> client's local proxy on :80 ──> localhost:3030
       (no tunnel hop)
```

One persistent WebSocket per client. All routes for that client multiplex
over that single connection using stream IDs (see `internal/tunnel/`).

## Request paths in detail

### Public HTTP request

`https://app.example.com/path` from anywhere on the internet:

1. **TLS terminates** at Cloudflare / your reverse proxy / whatever fronts the
   server. Server speaks plain HTTP on port 80.
2. **`server.ServeHTTP`** (`server.go:108`) checks the request path:
   - `/tunnel` → WebSocket upgrade for clients (skip — we're a public req)
   - `/health` → JSON health endpoint
   - subdomain == `admin_subdomain` → admin UI
   - everything else → `handleHTTPProxy`
3. **`handleHTTPProxy`** (`http_proxy.go:15`) extracts subdomain from `Host`
   header, finds the route in `s.clients[].routes` (linear scan, RLocked).
4. **Auth check** if route has `Auth` (see `references/auth.md`).
5. **WebSocket upgrade pass-through** if `Upgrade: websocket` — uses
   `handleWebSocketProxy` to hijack the connection and bidirectionally pump
   bytes through a tunnel stream.
6. **Otherwise** open a multiplexed tunnel stream:
   - Serialize the HTTP request (`r.Write(pipe)`) and send through the stream
   - Read response back from the stream and `http.ReadResponse` it
   - Copy headers + body to the public client

### Public TCP request

`psql -h server.example.com -p 15432` from any machine:

1. Server is listening on `:15432` because some client claimed a TCP route
   that hashed to that port. The listener is in
   `s.tcpListeners[<route-name>]`.
2. **`handlePlainTCPConn`** (`tcp_proxy.go:77`) finds the route, opens a
   tunnel stream to its owning client, bidirectionally pumps bytes.

### Same-machine HTTP request

`curl http://app.local.test` from the client host itself:

1. Goes to the **local proxy on :80** in `local_proxy.go:41`. Note: this is
   a separate listener from the tunnel server's :80, since they're on
   different machines.
2. Local proxy extracts subdomain from `Host`, finds the route in the same
   in-memory routes slice the client uses, enforces bearer/header auth
   (session auth is skipped here on purpose).
3. `httputil.NewSingleHostReverseProxy` to `route.Target`. No tunnel hop.

For this to work you need `/etc/hosts`-style entries pointing the
`*.local.test` (or whatever base domain you pick) at `127.0.0.1`.

### Same-machine TCP request

`psql -h 127.0.0.1 -p 15432` from the client host:

1. Local TCP proxy is listening on `route.local_port` (per-route, opt-in via
   the `local_port:` field in the config).
2. Direct `net.Dial(route.Target)` and pumps bytes. No tunnel.

### Third-machine TCP via `etunl connect`

`etunl connect --name db1 --local-port 5432` on some third machine, then
`psql -h 127.0.0.1 -p 5432`:

1. `etunl connect` opens its own local TCP listener.
2. For each accepted connection, opens a **brand-new WebSocket** to the
   server (no persistent registration; the `X-Machine-Name` header is
   omitted so the server treats it as anonymous in `server.go:170`).
3. Opens a stream for the route name and pumps bytes both ways.
4. When the connection closes, the WebSocket closes.

## Route ownership

The server's `s.clients` map is keyed by `machine_name`. When a client sends
its `RouteSync`, the server scans every other connected machine's routes
and **rejects** any route name already owned by another machine
(`server.go:208`). The local client never sees the rejection — it'll keep
showing the route in `etunl list` — but the server simply won't have it,
and requests to that subdomain will 404.

If two machines try to claim the same route, **first one wins** and stays
the winner until it disconnects. After disconnect, the other machine's
next sync will succeed.

## TCP port allocation

`allocatePort` (`server.go:73`) hashes the route name with FNV-1a, mods into
`tcp_port_range`. On collision (another route already on that port),
linear-scans for the next free port in the range.

This is **not** persistent — restarting the server may give a TCP route a
different port if the deterministic slot is now taken by something that
synced first. Plan firewall rules around the range, not specific ports, or
read the current ports from `curl <server>/health`.

## Reconnect semantics

Client side:

- WebSocket dial → on success, `SendRouteSync(routes)`, then `ReadLoop`.
- On any disconnect (network, server restart, token mismatch, route
  collision), `connectLoop` sleeps with exponential backoff (cap 30s) and
  retries.
- Config changes during disconnect are picked up on the next reconnect —
  the watcher updates `c.watcher.Config()` regardless.
- `c.conn = conn` is set on each successful connect, so a route change
  during a disconnect period might log "failed to sync routes" on the
  ghost `nil` conn — harmless, the new conn will sync on reconnect.

Server side:

- If a client reconnects with the same `machine_name` while the previous
  connection is still tracked, the old conn is closed
  (`server.go:182` — "machine X reconnecting; closing previous connection").
- After a disconnect, the client's routes are removed from `s.clients` and
  `syncTCPListeners` runs to tear down any TCP listeners that no machine
  owns anymore.

## What flows where (cheat sheet)

| Concern | Lives on |
|---|---|
| Route definitions (truth) | Client `config.yaml` |
| Route → machine mapping | Server `s.clients` (in-memory) |
| Auth secrets | Client config; pushed to server in-memory on sync |
| TCP port → route mapping | Server `s.tcpListeners` (in-memory, deterministic from `tcp_port_range`) |
| HTTP request bytes | Server pipes them through the tunnel stream |
| Same-machine traffic | Never touches the tunnel — local proxy only |
| Metrics | Server `s.metrics` (in-memory, exposed via admin) |
