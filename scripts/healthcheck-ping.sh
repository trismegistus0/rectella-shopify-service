#!/bin/bash

# End-to-end healthcheck: hits /health through the public Cloudflare tunnel,
# then pings healthchecks.io to signal liveness. If anything is wrong (NUC
# down, internet out, tunnel dead, service crashed, DB unreachable) the ping
# is missed and healthchecks.io alerts the operator.
#
# Required env (from .env):
#   HC_PING_URL_HEALTH — base URL like https://hc-ping.com/<uuid>
# Optional env:
#   HEALTH_URL — override the URL to probe (default: discover from cloudflared logs)

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ -f .env ]]; then
  set -a
  . ./.env
  set +a
fi

HC_URL="${HC_PING_URL_HEALTH:-}"
[[ -z "$HC_URL" ]] && { echo "HC_PING_URL_HEALTH not set, skipping"; exit 0; }

# URL discovery (when HEALTH_URL not pre-set), in precedence order:
#   1. Cloudflare named-tunnel hostname from ~/.cloudflared/config.yml
#      (production — stable URL, doesn't drift across cloudflared restarts)
#   2. Last trycloudflare.com URL emitted by cloudflared (legacy Quick
#      Tunnel — kept as a dev/test fallback)
#   3. localhost:9080 (loopback fallback — proves the service is up
#      but not the public path)
HEALTH_URL="${HEALTH_URL:-}"
if [[ -z "$HEALTH_URL" ]]; then
  CONFIG_YML="$HOME/.cloudflared/config.yml"
  if [[ -r "$CONFIG_YML" ]]; then
    NAMED_HOST=$(grep -oP 'hostname:\s*\K\S+' "$CONFIG_YML" | head -1) || true
    if [[ -n "$NAMED_HOST" ]]; then
      HEALTH_URL="https://${NAMED_HOST}/health"
    fi
  fi
fi
if [[ -z "$HEALTH_URL" ]]; then
  TUNNEL=$(journalctl --user -u cloudflared --since "7 days ago" -o cat 2>/dev/null \
    | grep -oP 'https://[a-z0-9]+(?:-[a-z0-9]+)+\.trycloudflare\.com' | tail -1) || true
  if [[ -n "$TUNNEL" ]]; then
    HEALTH_URL="${TUNNEL}/health"
  else
    HEALTH_URL="http://localhost:9080/health"
  fi
fi

# Probe — must return 200 with status:ok within 10s
if curl -fsS --max-time 10 "$HEALTH_URL" 2>/dev/null | grep -q '"status":"ok"'; then
  curl -fsS --max-time 10 --retry 3 "$HC_URL" >/dev/null 2>&1 || true
  echo "healthcheck-ping: ok ($HEALTH_URL)"
else
  curl -fsS --max-time 10 --retry 3 "${HC_URL}/fail" >/dev/null 2>&1 || true
  echo "healthcheck-ping: FAIL ($HEALTH_URL)" >&2
  exit 1
fi
