#!/bin/bash

# Rectella VPN health monitor — checks and heals network issues.
#
# Designed to run periodically (cron or Claude Code CronCreate).
# Only acts when VPN is supposed to be up (PID file exists).
#
# Usage:
#   ./scripts/vpn-monitor.sh          # Check + heal
#   ./scripts/vpn-monitor.sh --quiet  # Suppress output unless action taken

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."  # Project root — ensures .env is findable for vpn.sh reconnect.
PID_FILE="/tmp/rectella-vpn.pid"
HOSTS_MARKER="rectella-vpn"
QUIET=false

[[ "${1:-}" == "--quiet" ]] && QUIET=true

log() {
  $QUIET && return
  echo "[vpn-monitor] $(date '+%H:%M:%S') $*"
}

warn() {
  echo "[vpn-monitor] $(date '+%H:%M:%S') WARN: $*"
}

healed() {
  echo "[vpn-monitor] $(date '+%H:%M:%S') HEALED: $*"
}

# Only monitor when VPN is supposed to be up.
if [[ ! -f "$PID_FILE" ]]; then
  log "VPN not active (no PID file). Nothing to monitor."
  exit 0
fi

vpn_pid=$(cat "$PID_FILE")
issues=0
fixes=0

# 1. Check openconnect process is alive.
if ! sudo kill -0 "$vpn_pid" 2>/dev/null; then
  warn "openconnect process $vpn_pid is dead"
  issues=$((issues + 1))

  warn "Attempting VPN reconnect..."
  "$SCRIPT_DIR/vpn.sh" down 2>/dev/null || true
  if "$SCRIPT_DIR/vpn.sh" up 2>/dev/null; then
    healed "VPN reconnected"
    fixes=$((fixes + 1))
  else
    warn "VPN reconnect FAILED — manual intervention needed"
    notify-send -u critical "VPN Monitor" "Rectella VPN reconnect failed" 2>/dev/null || true
  fi
  # vpn.sh up runs fix_dns + fix_hosts + test, so exit after reconnect.
  exit $((issues - fixes))
fi

# 2. Check tun0 interface exists.
if ! ip link show tun0 &>/dev/null; then
  warn "tun0 interface missing but openconnect running — restarting"
  issues=$((issues + 1))

  "$SCRIPT_DIR/vpn.sh" down 2>/dev/null || true
  if "$SCRIPT_DIR/vpn.sh" up 2>/dev/null; then
    healed "VPN reconnected (tun0 was missing)"
    fixes=$((fixes + 1))
  else
    warn "VPN reconnect FAILED"
    notify-send -u critical "VPN Monitor" "Rectella VPN reconnect failed" 2>/dev/null || true
  fi
  exit $((issues - fixes))
fi

# 3. Check DNS routing domain has ~ prefix.
tun0_domains=$(resolvectl domain tun0 2>/dev/null | sed 's/^.*: //' || echo "")
if [[ -n "$tun0_domains" && "$tun0_domains" != *"~"* ]]; then
  warn "DNS routing domain missing ~ prefix: $tun0_domains"
  issues=$((issues + 1))

  routing_domains=""
  for d in $tun0_domains; do
    if [[ $d == ~* ]]; then
      routing_domains="$routing_domains $d"
    else
      routing_domains="$routing_domains ~$d"
    fi
  done

  if sudo resolvectl domain tun0 $routing_domains 2>/dev/null && \
     sudo resolvectl default-route tun0 false 2>/dev/null; then
    healed "DNS routing domain fixed ($routing_domains)"
    fixes=$((fixes + 1))
  else
    warn "Failed to fix DNS routing domain"
  fi
fi

# 4. Check /etc/hosts entries exist.
if ! grep -q "BEGIN $HOSTS_MARKER" /etc/hosts 2>/dev/null; then
  warn "/etc/hosts entries missing"
  issues=$((issues + 1))

  # Delegate to vpn.sh which owns the host entries (single source of truth).
  if "$SCRIPT_DIR/vpn.sh" fix-hosts 2>/dev/null; then
    healed "/etc/hosts entries restored"
    fixes=$((fixes + 1))
  else
    warn "Failed to restore /etc/hosts entries"
  fi
fi

# 5. Check Mullvad is still protecting external traffic.
mullvad_status=$(mullvad status 2>/dev/null || echo "unknown")
if ! echo "$mullvad_status" | grep -q "Connected"; then
  warn "Mullvad not connected: $mullvad_status"
  issues=$((issues + 1))
  notify-send -u critical "VPN Monitor" "Mullvad disconnected!" 2>/dev/null || true
  # Don't auto-fix Mullvad — user's responsibility.
fi

# 6. Check external IP matches Mullvad (no leak).
external_ip=$(curl -4 -s --max-time 5 ifconfig.me 2>/dev/null || echo "")
mullvad_ip=$(mullvad status 2>/dev/null | grep -oP 'IPv4: \K[0-9.]+' || echo "")
if [[ -n "$external_ip" && -n "$mullvad_ip" && "$external_ip" != "$mullvad_ip" ]]; then
  warn "IP LEAK DETECTED: external=$external_ip, expected Mullvad=$mullvad_ip"
  issues=$((issues + 1))
  notify-send -u critical "VPN Monitor" "IP leak detected! $external_ip" 2>/dev/null || true
fi

# Summary.
if (( issues == 0 )); then
  log "All checks passed"
elif (( fixes == issues )); then
  log "All $issues issues auto-healed"
else
  warn "$((issues - fixes)) unresolved issues (healed $fixes/$issues)"
fi

exit $((issues - fixes))
