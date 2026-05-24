# etunl troubleshooting

Common failure modes, what they look like, and the root-cause fix. Always
prefer the root cause over masking with a daemon restart.

## "route not found: X" / 404 on a subdomain

The server received the request but doesn't know about the route.

**Check, in order:**

1. **Is the client connected?** `curl <server>/health` — count `machines[]`.
2. **Does the client claim this route?** `etunl list` on the client.
3. **Was the route rejected for ownership conflict?** Search server log
   for `route "X" from "<client>" rejected: already owned by "<other>"`.
   Resolve by renaming on one side or disconnecting the older owner.
4. **Did sync fail silently?** `journalctl -u etunl` on the client; look
   for `failed to sync routes: ...`. Often means the WebSocket dropped
   mid-sync.

**Fix:** rename the colliding route (`etunl remove` then `etunl add` with
a unique name) or disconnect the other claimant.

## 502 Bad Gateway on an HTTP route

The route is registered, but the request couldn't complete through the
tunnel.

Common causes:

- The client's upstream (`localhost:3000` or whatever) is **down**.
  Reproduce locally with `curl http://localhost:3000` on the client.
- The tunnel WebSocket dropped mid-request. One-shot transient; retry once.
- The upstream returned an invalid HTTP response (truncated headers, etc.).
  The server logs `bad gateway: <parse error>`.

**Fix:** start the upstream, or correct the upstream's HTTP output.

## 401 Unauthorized that you didn't expect

Auth is misconfigured on the route. Inspect with care — don't print the
secret:

```bash
# Tell us only the AUTH SCHEME, not the value:
curl -s <server>/health | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('machines',[]):
  for r in m.get('routes',[]):
    print(r['name'], '->', r.get('auth') or '(none)')
"
```

- `auth: 'bearer'` expects `Authorization: Bearer <value>`
- `auth: 'session'` expects a valid signed cookie (visit `/___login___`)
- `auth: '<some-header-name>'` expects `<some-header-name>: <value>`

If a session login redirects to `/___login___` infinitely:

- `session_secret` was rotated since the user logged in. They need to
  log in again.
- The Set-Cookie domain isn't reaching the browser. Check that Cloudflare /
  your proxy isn't stripping the cookie.

## TCP route never opens — `nc -vz <server> <port>` refused

1. Is `tcp_port_range` set in `server.yaml`? **Empty range silently
   disables TCP** (you'll see one WARNING at server start; nothing after).
2. Did the server allocate a listener? `curl <server>/health` returns
   `tcp_ports`. If your route is missing, no listener exists.
3. Did the listener bind? `ss -ltn` on the server. If the port is in use,
   the allocator linear-scans, so the actual port may not be the one you
   expected — check `tcp_ports` again.
4. Is your firewall passing the port? Cloudflare doesn't proxy raw TCP on
   arbitrary ports — use Spectrum, a separate hostname, or bypass
   Cloudflare for TCP.

## Local proxy fails to bind :80 — `permission denied`

The client's local proxy needs root or `CAP_NET_BIND_SERVICE` to bind
port 80.

```
local HTTP proxy failed: listen tcp :80: bind: permission denied
```

Three options:

- Run the client as root (systemd: `User=root`).
- `sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/etunl`
- Skip the same-machine local proxy and route only through the tunnel —
  there's no flag to disable it cleanly, so accept the warning.

## Client stuck in reconnect loop

```
tunnel disconnected: websocket: bad handshake (reconnecting in 2s)
tunnel disconnected: websocket: bad handshake (reconnecting in 4s)
tunnel disconnected: websocket: bad handshake (reconnecting in 8s)
```

- **`bad handshake` + `401`**: token mismatch. Check `~/.etunl/config.yaml`
  on the client matches `~/.etunl/server.yaml`'s `token` on the server.
- **`dial tcp: i/o timeout`**: server unreachable. DNS or firewall.
- **`dial tcp: connection refused`**: server daemon not running.

## "machine X reconnecting; closing previous connection" floods the log

The same `machine_name` is being claimed by two processes. Usually means:

- Two `etunl client` processes are running on the same host (check with
  `ps aux | grep etunl`).
- A previous client wasn't cleanly shut down and is still mid-disconnect
  when the new one comes up.

**Fix:** `pkill etunl` then start exactly one client.

## Dashboard returns "Setup required" forever

The server admin UI hasn't had its initial credentials set. POST to
`/api/setup` once:

```bash
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"user":"admin","password":"<at-least-8-chars>"}' \
  http://manage.<your-domain>/api/setup
```

After that, `/api/setup` returns 409 forever and the dashboard requires
basic auth on every other path.

## "TCP port range not configured" in server logs

Exactly what it says — `tcp_port_range` in `server.yaml` is empty, or the
config file was hand-written without it. The server keeps running for HTTP
but rejects any TCP route claims. Edit `server.yaml`:

```yaml
tcp_port_range: "15000-15100"
```

Then restart `etunl server` (one of the few times restart is the right
move — port range is read at startup, not hot-reloaded).

## Routes don't update after editing config.yaml

The fsnotify watcher dropped the file. Common when editors do
write-replace (truncate-and-write-new-inode) instead of in-place rewrite.

**Fix:** save the file once more after the editor finishes. Or use
`etunl add` / `etunl remove`, which write through a stable inode.

## Token leaked — what now?

1. On the server, rotate via the admin UI (`POST /api/rotate-token`) or by
   editing `server.yaml` and restarting `etunl server`.
2. On **every** client, re-init: `etunl init --mode client --server
   <addr> <new-token>` (overwrites `config.yaml`).
3. Restart each `etunl client`.

Until every client has the new token, they'll all be in reconnect-loop
hell. Plan the rotation accordingly.

## Where to look when nothing fits

- **Server logs**: `journalctl -u etunl` on the public box, or the
  terminal running `etunl server`.
- **Client logs**: `journalctl -u etunl` on the local box.
- **Server health**: `curl <server>/health` — connected machines, route
  list, allocated TCP ports.
- **Client dashboard**: `curl localhost:8080/api/status` — connection
  state, routes from the client's perspective.
- **`scripts/diagnose.sh <route>`** in this skill — walks every layer.
