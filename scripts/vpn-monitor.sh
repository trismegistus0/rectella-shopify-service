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
SYSPRO_HOST="192.168.3.150"
SYSPRO_PORT="31002"
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

# Determine if VPN is supposed to be up. Two management paths:
#   (a) systemd — rectella-vpn.service is enabled → VPN should be alive.
#   (b) manual  — vpn.sh up wrote /tmp/rectella-vpn.pid.
# Monitor is a no-op only when neither applies.
vpn_enabled=false
if systemctl --user is-enabled --quiet rectella-vpn.service 2>/dev/null; then
  vpn_enabled=true
fi

if ! $vpn_enabled && [[ ! -f "$PID_FILE" ]]; then
  log "VPN not active (not enabled, no PID file). Nothing to monitor."
  exit 0
fi

issues=0
fixes=0

# 0. If the systemd service is enabled but the unit has stopped, start it.
# This handles the cold-dead case: openconnect exited overnight (sleep,
# network change) and systemd's Restart= already gave up.
if $vpn_enabled && ! systemctl --user is-active --quiet rectella-vpn.service; then
  warn "rectella-vpn.service enabled but inactive — starting"
  issues=$((issues + 1))
  if systemctl --user start rectella-vpn.service; then
    sleep 6
    if systemctl --user is-active --quiet rectella-vpn.service; then
      healed "rectella-vpn.service started"
      fixes=$((fixes + 1))
    else
      warn "rectella-vpn.service failed to become active"
      notify-send -u critical "VPN Monitor" "rectella-vpn.service won't start" 2>/dev/null || true
      exit $((issues - fixes))
    fi
  else
    warn "systemctl start failed"
    exit $((issues - fixes))
  fi
fi

# Pick up the PID for downstream liveness checks.
vpn_pid=""
if [[ -f "$PID_FILE" ]]; then
  vpn_pid=$(cat "$PID_FILE" 2>/dev/null || true)
fi
if [[ -z "$vpn_pid" ]] && $vpn_enabled; then
  vpn_pid=$(systemctl --user show -p MainPID --value rectella-vpn.service 2>/dev/null || echo "")
fi
if [[ -z "$vpn_pid" || "$vpn_pid" == "0" ]]; then
  warn "cannot determine VPN PID — skipping liveness check"
  exit $((issues - fixes))
fi

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

# 5. Check SYSPRO is actually reachable through the tunnel.
# Catches the stale-tunnel failure mode: tun0 exists, openconnect alive,
# but packets silently black-hole (e.g. after ethernet → wifi switch where
# openconnect's underlying route went away).
if ! timeout 5 bash -c "(exec 3<>/dev/tcp/$SYSPRO_HOST/$SYSPRO_PORT) 2>/dev/null"; then
  warn "SYSPRO unreachable ($SYSPRO_HOST:$SYSPRO_PORT) — tunnel broken"
  issues=$((issues + 1))

  # Prefer systemd-managed restart if available.
  if systemctl --user is-active --quiet rectella-vpn.service 2>/dev/null; then
    warn "Restarting rectella-vpn.service..."
    systemctl --user restart rectella-vpn.service || true
    sleep 6
  else
    "$SCRIPT_DIR/vpn.sh" down 2>/dev/null || true
    "$SCRIPT_DIR/vpn.sh" up 2>/dev/null || true
  fi

  if timeout 5 bash -c "(exec 3<>/dev/tcp/$SYSPRO_HOST/$SYSPRO_PORT) 2>/dev/null"; then
    healed "SYSPRO reachable after VPN restart"
    fixes=$((fixes + 1))
  else
    warn "SYSPRO still unreachable after VPN restart"
    notify-send -u critical "VPN Monitor" "SYSPRO unreachable — manual check needed" 2>/dev/null || true
  fi
fi

# 6a. Check Mullvad is still protecting external traffic (only if local CLI).
# On the NUC, Mullvad runs on the Flint 3 router — no local mullvad CLI and
# these checks would false-positive. Skip cleanly when the CLI isn't present.
# Mullvad state is a user choice on dev workstations; log to journal only,
# never notify or count as an unhealed issue. IP-leak (6b) stays noisy.
if command -v mullvad >/dev/null 2>&1; then
  mullvad_status=$(mullvad status 2>/dev/null || echo "unknown")
  mullvad_connected=false
  if echo "$mullvad_status" | grep -q "Connected"; then
    mullvad_connected=true
  else
    log "Mullvad not connected (user state): $mullvad_status"
  fi

  # 6b. Check external IP matches Mullvad (no leak). Only meaningful when Mullvad is up.
  if $mullvad_connected; then
    external_ip=$(curl -4 -s --max-time 5 ifconfig.me 2>/dev/null || echo "")
    mullvad_ip=$(mullvad status 2>/dev/null | grep -oP 'IPv4: \K[0-9.]+' || echo "")
    if [[ -n "$external_ip" && -n "$mullvad_ip" && "$external_ip" != "$mullvad_ip" ]]; then
      warn "IP LEAK DETECTED: external=$external_ip, expected Mullvad=$mullvad_ip"
      issues=$((issues + 1))
      notify-send -u critical "VPN Monitor" "IP leak detected! $external_ip" 2>/dev/null || true
    fi
  fi
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
