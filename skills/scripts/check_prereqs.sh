#!/usr/bin/env bash
#
# check_prereqs.sh — verify this host is ready to run etunl.
#
# Detects host role (server / client / third-party / not-installed), confirms
# the binary works, the config parses, the systemd unit (if any) is healthy,
# and the local dashboard / server /health endpoint responds.
#
# Read-only. Safe to run without confirmation.
#
# Usage:   ./check_prereqs.sh
# Exits 0 if everything passes, 1 if anything is missing or broken.

set -u

OK=0
WARN=0
FAIL=0

say()  { printf '  %s\n' "$*"; }
pass() { printf '  \033[32m✓\033[0m %s\n' "$*"; OK=$((OK+1)); }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; WARN=$((WARN+1)); }
fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }

section() { printf '\n\033[1m%s\033[0m\n' "$*"; }

CLIENT_CFG="${HOME}/.etunl/config.yaml"
SERVER_CFG="${HOME}/.etunl/server.yaml"

# ---------------------------------------------------------------- binary

section "Binary"

if ! command -v etunl >/dev/null 2>&1; then
  fail "etunl not on PATH"
  say  "Install: curl -fsSL https://raw.githubusercontent.com/iluxav/ntunl/main/install.sh | sh"
  exit 1
fi
pass "etunl found at $(command -v etunl)"

if VER=$(etunl --version 2>/dev/null); then
  pass "$VER"
else
  warn "etunl --version did not print anything"
fi

# -------------------------------------------------------------- host role

section "Host role"

ROLE="third-party"
if [[ -f "$SERVER_CFG" ]]; then
  ROLE="server"
elif [[ -f "$CLIENT_CFG" ]]; then
  ROLE="client"
fi
pass "detected role: $ROLE"

# --------------------------------------------------------------- config

section "Config"

case "$ROLE" in
  server)
    if [[ -r "$SERVER_CFG" ]]; then
      pass "$SERVER_CFG readable"
      perms=$(stat -f '%Lp' "$SERVER_CFG" 2>/dev/null || stat -c '%a' "$SERVER_CFG" 2>/dev/null || echo "?")
      if [[ "$perms" == "600" ]]; then
        pass "perms 600"
      else
        warn "perms $perms (expected 600 — contains tunnel token + admin password)"
      fi
      # cheap structural check; do NOT print contents
      if grep -q '^listen_http:' "$SERVER_CFG" && grep -q '^token:' "$SERVER_CFG"; then
        pass "listen_http and token keys present"
      else
        fail "server.yaml missing listen_http or token"
      fi
      if grep -q '^tcp_port_range:' "$SERVER_CFG"; then
        pass "tcp_port_range present (TCP tunneling enabled)"
      else
        warn "tcp_port_range absent — TCP tunneling will be DISABLED"
      fi
    else
      fail "$SERVER_CFG not readable"
    fi
    ;;

  client)
    if [[ -r "$CLIENT_CFG" ]]; then
      pass "$CLIENT_CFG readable"
      perms=$(stat -f '%Lp' "$CLIENT_CFG" 2>/dev/null || stat -c '%a' "$CLIENT_CFG" 2>/dev/null || echo "?")
      if [[ "$perms" == "600" ]]; then
        pass "perms 600"
      else
        warn "perms $perms (expected 600 — contains tunnel token)"
      fi
      if grep -q '^server:' "$CLIENT_CFG" && grep -q '^token:' "$CLIENT_CFG"; then
        pass "server and token keys present"
      else
        fail "config.yaml missing server or token"
      fi
      # route count without dumping route contents
      n=$(etunl list 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')
      pass "configured routes: ${n:-0}"
    else
      fail "$CLIENT_CFG not readable"
    fi
    ;;

  third-party)
    say "no etunl config on this host; OK for ad-hoc 'etunl connect' use"
    ;;
esac

# -------------------------------------------------------------- service

section "Service (Linux systemd)"

if command -v systemctl >/dev/null 2>&1; then
  if systemctl list-unit-files 2>/dev/null | grep -q '^etunl\.service'; then
    if systemctl is-active --quiet etunl 2>/dev/null; then
      pass "etunl.service active"
    else
      state=$(systemctl is-active etunl 2>/dev/null || echo "unknown")
      fail "etunl.service installed but not active (state: $state)"
      say  "→ sudo journalctl -u etunl --since '5 min ago' --no-pager"
    fi
  else
    if [[ "$ROLE" == "third-party" ]]; then
      say "no etunl.service (expected for third-party hosts)"
    else
      warn "no etunl.service — daemon must be started manually"
    fi
  fi
else
  say "systemd not present (probably macOS); skipping service check"
fi

# -------------------------------------------------------------- liveness

section "Liveness"

case "$ROLE" in
  server)
    if curl -fsS --max-time 3 http://localhost/health >/dev/null 2>&1; then
      pass "GET localhost/health responds"
      machines=$(curl -fsS --max-time 3 http://localhost/health 2>/dev/null | grep -o '"machine_name"' | wc -l | tr -d ' ')
      pass "connected machines: ${machines:-0}"
    else
      fail "localhost/health unreachable (server daemon down? port 80 blocked?)"
    fi
    ;;

  client)
    if curl -fsS --max-time 3 http://localhost:8080/api/status >/dev/null 2>&1; then
      pass "dashboard responds on localhost:8080"
    else
      warn "dashboard not responding on :8080 (client daemon down, or --dashboard disabled)"
    fi
    server_host=$(awk -F: '/^server:/ {gsub(/ /,"",$2); print $2; exit}' "$CLIENT_CFG" 2>/dev/null)
    if [[ -n "${server_host:-}" ]]; then
      if curl -fsS --max-time 5 "http://${server_host}/health" >/dev/null 2>&1; then
        pass "tunnel server ${server_host}/health reachable"
      else
        fail "tunnel server ${server_host}/health UNREACHABLE — client cannot connect"
      fi
    fi
    ;;
esac

# --------------------------------------------------------------- summary

section "Summary"
say "passes: $OK   warnings: $WARN   failures: $FAIL"
if (( FAIL > 0 )); then
  exit 1
fi
exit 0
