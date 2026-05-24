#!/usr/bin/env bash
#
# diagnose.sh — triage a single broken etunl route in one shot.
#
# Walks every layer the request crosses: client config → tunnel → server
# registry → upstream target. Stops at the first hard failure and points at
# the layer most likely responsible.
#
# Read-only. Safe to run without confirmation.
#
# Usage:   ./diagnose.sh <route-name>
#          ./diagnose.sh app
#          ./diagnose.sh db1
#
# Run on whichever side you have access to. The script auto-detects whether
# this host is the client (has ~/.etunl/config.yaml) or the server (has
# ~/.etunl/server.yaml) and runs the appropriate layer checks.

set -u

ROUTE="${1:-}"
if [[ -z "$ROUTE" ]]; then
  echo "usage: $0 <route-name>" >&2
  echo "       (e.g. 'app', 'db1', whatever appears in 'etunl list')" >&2
  exit 2
fi

CLIENT_CFG="${HOME}/.etunl/config.yaml"
SERVER_CFG="${HOME}/.etunl/server.yaml"

say()    { printf '  %s\n' "$*"; }
ok()     { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad()    { printf '  \033[31m✗\033[0m %s\n' "$*"; }
warn()   { printf '  \033[33m!\033[0m %s\n' "$*"; }
header() { printf '\n\033[1m%s\033[0m\n' "$*"; }

ROLE="unknown"
[[ -f "$CLIENT_CFG" ]] && ROLE="client"
[[ -f "$SERVER_CFG" ]] && ROLE="server"

header "Route: ${ROUTE}   Host role: ${ROLE}"

# ===========================================================================
# CLIENT-SIDE CHECKS
# ===========================================================================
if [[ "$ROLE" == "client" ]]; then

  header "1. Is the route in the client config?"
  if ! command -v etunl >/dev/null 2>&1; then
    bad "etunl binary not on PATH — cannot inspect config"
    exit 1
  fi

  ROUTE_LINE=$(etunl list 2>/dev/null | awk -v r="$ROUTE" 'NR>1 && $1==r {print}')
  if [[ -z "$ROUTE_LINE" ]]; then
    bad "route '${ROUTE}' not in client config"
    say "→ etunl add --name ${ROUTE} --type http --target localhost:<port>"
    exit 1
  fi
  ok "found: $ROUTE_LINE"
  ROUTE_TYPE=$(echo "$ROUTE_LINE" | awk '{print $2}')
  ROUTE_TARGET=$(echo "$ROUTE_LINE" | awk '{print $3}')
  ROUTE_LOCAL_PORT=$(echo "$ROUTE_LINE" | awk '{print $4}')

  header "2. Is the upstream target reachable on this machine?"
  case "$ROUTE_TYPE" in
    http)
      if curl -fsS --max-time 3 -o /dev/null "http://${ROUTE_TARGET}/"; then
        ok "GET http://${ROUTE_TARGET}/ — 2xx/3xx"
      else
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://${ROUTE_TARGET}/" || echo "000")
        if [[ "$code" == "000" ]]; then
          bad "cannot connect to http://${ROUTE_TARGET}/ (target service is DOWN)"
        else
          warn "http://${ROUTE_TARGET}/ returned ${code} (upstream is up; app-level error)"
        fi
      fi
      ;;
    tcp)
      host="${ROUTE_TARGET%%:*}"; port="${ROUTE_TARGET##*:}"
      if (echo > "/dev/tcp/${host}/${port}") 2>/dev/null; then
        ok "TCP connect to ${ROUTE_TARGET} succeeds"
      else
        bad "cannot connect to ${ROUTE_TARGET} (target service is DOWN)"
      fi
      ;;
  esac

  header "3. Is the client connected to the tunnel server?"
  if curl -fsS --max-time 3 http://localhost:8080/api/status >/dev/null 2>&1; then
    ok "dashboard responds on :8080"
    connected=$(curl -fsS --max-time 3 http://localhost:8080/api/status 2>/dev/null | grep -o '"connected":[a-z]*' | head -1)
    say "  status snippet: ${connected:-<unparsed>}"
  else
    warn "dashboard not responding — client daemon may be down"
    say "→ sudo systemctl status etunl   (or check the terminal you ran 'etunl client' in)"
  fi

  header "4. Did the server accept this route?"
  SERVER_HOST=$(awk -F: '/^server:/ {gsub(/ /,"",$2); print $2; exit}' "$CLIENT_CFG" 2>/dev/null)
  if [[ -z "$SERVER_HOST" ]]; then
    warn "could not parse 'server:' from $CLIENT_CFG"
  else
    if HEALTH=$(curl -fsS --max-time 5 "http://${SERVER_HOST}/health" 2>/dev/null); then
      if echo "$HEALTH" | grep -q "\"name\":\"${ROUTE}\""; then
        ok "server reports route '${ROUTE}' is registered"
      else
        bad "route '${ROUTE}' NOT in server /health — likely owned by another machine, or sync failed"
        say "→ check server logs for: route \"${ROUTE}\" from \"<your-machine>\" rejected: already owned by \"<other>\""
      fi
    else
      bad "cannot reach http://${SERVER_HOST}/health — server is down or DNS is wrong"
    fi
  fi

  if [[ "$ROUTE_TYPE" == "http" ]]; then
    header "5. Does the local proxy serve this route?"
    if curl -fsS --max-time 3 -H "Host: ${ROUTE}.local.test" -o /dev/null http://localhost/ 2>/dev/null; then
      ok "local proxy on :80 responds for ${ROUTE}.<any>"
    else
      warn "local proxy on :80 didn't respond (it needs root/CAP_NET_BIND to bind :80; this is OK if you only use the tunnel)"
    fi
  fi

  if [[ "$ROUTE_TYPE" == "tcp" && "$ROUTE_LOCAL_PORT" != "-" && -n "$ROUTE_LOCAL_PORT" ]]; then
    header "5. Is the same-machine local TCP proxy listening?"
    if (echo > "/dev/tcp/127.0.0.1/${ROUTE_LOCAL_PORT}") 2>/dev/null; then
      ok "127.0.0.1:${ROUTE_LOCAL_PORT} accepts connections"
    else
      bad "127.0.0.1:${ROUTE_LOCAL_PORT} not listening — client may be down or port already taken"
    fi
  fi
fi

# ===========================================================================
# SERVER-SIDE CHECKS
# ===========================================================================
if [[ "$ROLE" == "server" ]]; then

  header "1. Is the server daemon up?"
  if HEALTH=$(curl -fsS --max-time 3 http://localhost/health 2>/dev/null); then
    ok "GET localhost/health responds"
  else
    bad "localhost/health unreachable — etunl server not running, or :80 conflict"
    say "→ sudo systemctl status etunl"
    exit 1
  fi

  header "2. Is anyone connected?"
  N_MACHINES=$(echo "$HEALTH" | grep -o '"machine_name"' | wc -l | tr -d ' ')
  if (( N_MACHINES == 0 )); then
    bad "no clients connected — nothing can route"
    exit 1
  fi
  ok "${N_MACHINES} client(s) connected"

  header "3. Is route '${ROUTE}' registered?"
  if echo "$HEALTH" | grep -q "\"name\":\"${ROUTE}\""; then
    ok "route is registered"
    # tease out the owner machine — relies on the JSON layout being machines[].routes[].name
    OWNER=$(echo "$HEALTH" | python3 -c "
import sys,json
h=json.load(sys.stdin)
for m in h.get('machines',[]):
  for r in m.get('routes',[]):
    if r.get('name')=='${ROUTE}':
      print(m.get('machine_name','?')); sys.exit(0)
" 2>/dev/null)
    [[ -n "$OWNER" ]] && say "  owner machine: $OWNER"
  else
    bad "route '${ROUTE}' is NOT registered by any connected client"
    say "→ on the client: etunl list | grep ${ROUTE}"
    say "→ on the client: journalctl -u etunl | grep -i 'route sync'"
  fi

  header "4. For TCP routes — is the listener up?"
  PORT=$(echo "$HEALTH" | python3 -c "
import sys,json
h=json.load(sys.stdin)
print(h.get('tcp_ports',{}).get('${ROUTE}','') or '')
" 2>/dev/null)
  if [[ -n "$PORT" ]]; then
    ok "TCP listener for '${ROUTE}' on port ${PORT}"
    if ss -ltn 2>/dev/null | grep -q ":${PORT}\b"; then
      ok "port ${PORT} is in LISTEN state"
    else
      bad "server says listener should be on ${PORT}, but nothing is listening — restart 'etunl server'?"
    fi
  else
    say "(no TCP listener for this route — expected if route is HTTP)"
  fi
fi

if [[ "$ROLE" == "unknown" ]]; then
  bad "no etunl config on this host. Run on the client or the server."
  exit 1
fi

header "Done."
