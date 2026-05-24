# etunl HTTP route auth

HTTP routes can be protected by **exactly one** of three forms. TCP routes
have no auth — protect them at the application level (DB passwords, mTLS,
etc.). Setting `auth` on a TCP route is a config error.

Forms are mutually exclusive: setting more than one fails validation
(`config.go:Auth.Validate`).

## Form A — Bearer token

Standard `Authorization: Bearer <token>` header. Ideal for API clients you
control.

```yaml
- name: api
  type: http
  target: localhost:8080
  auth:
    bearer: "long-random-secret"
```

Requests without a matching header get `401 Unauthorized`. Enforced at both
the **public server** (rejected before reaching the tunnel) and the **local
proxy** (rejected before reaching the upstream).

## Form B — Custom header

Useful when the upstream system requires a specific header name like
`X-API-Key` or `X-Webhook-Secret`.

```yaml
- name: webhook
  type: http
  target: localhost:9000
  auth:
    header: "X-API-Key"
    value: "long-random-secret"
```

Both `header` and `value` are required together. Enforcement identical to
Form A — server + local proxy both reject mismatched/missing headers.

## Form C — Cookie session (browser login)

For human-facing apps. The server intercepts unauthenticated requests,
redirects to `/___login___` on the route's host, serves a login form,
verifies credentials, and sets a signed cookie. Subsequent requests carry
the cookie and pass through transparently.

```yaml
- name: app
  type: http
  target: localhost:3030
  auth:
    users:
      - user: ilya
        password: "change-me"
      - user: someone
        password: "other-pwd"
```

Endpoints injected by the server:

| Path | Purpose |
|---|---|
| `/___login___` | GET serves form; POST checks credentials, sets cookie, redirects |
| `/___logout___` | Clears cookie and redirects to `/` |

The cookie is HMAC-signed with `server.yaml`'s `session_secret`. Rotating
`session_secret` invalidates every active session.

**Important: session auth is enforced ONLY on the public server.** The
local proxy (port 80 on the client machine) leaves session-protected routes
**open** because the cookie domain wouldn't match a `local.test`-style host
anyway. This is a feature — your same-machine browser doesn't need to log
in to hit `app.local.test` — but understand the implication: anyone on the
same machine has unauthenticated access to session-protected routes.

If you want auth enforcement on the local proxy too, use Form A or B
instead.

## Where each form is enforced

| Form | Public server | Local proxy (same-machine) |
|---|---|---|
| (none) | open | open |
| Bearer | enforced | enforced |
| Header | enforced | enforced |
| Session | enforced | **open** (by design) |

## How auth travels

Auth config is sent to the server as part of `RouteSync`. The server stores
it in memory only — it's never persisted server-side. Restarting the server
loses no auth state because the next client reconnect re-syncs everything.
However: rotating `session_secret` on the server **does** invalidate
existing session cookies (the signature won't verify).

## Password storage

Session auth user passwords in `~/.etunl/config.yaml` are **plaintext**.
Mode 0600 + a trusted client host is the only thing protecting them. If
that's not acceptable for your threat model, use Form A or B with a
properly rotated secret instead.

The admin UI's `admin_password` in `server.yaml` is **also plaintext**
(stored as-is, compared with `subtle.ConstantTimeCompare` in
`admin.go:78`). Same caveat applies.

## Common mistakes

- Setting `auth.bearer` AND `auth.header` — fails with `auth: set only one
  of bearer, header+value, or users`.
- Setting `auth.header` without `auth.value` (or vice versa) — fails with
  `auth: custom header form requires both header and value`.
- Putting `auth` on a TCP route — fails with `auth is only supported on
  http routes`.
- Setting `auth.users` with an entry that has empty `user` or `password` —
  fails with `auth: users[N] requires both user and password`.
- Expecting session auth to gate same-machine access via the local proxy —
  it doesn't. See table above.
