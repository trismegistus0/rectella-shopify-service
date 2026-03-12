#!/bin/bash

# Connect/disconnect Rectella VPN alongside Mullvad.
#
# Uses mullvad-exclude to run openconnect outside the Mullvad tunnel.
# Mullvad stays on permanently — no disconnect/reconnect dance.
#
# Usage: ./scripts/vpn.sh up|down|status|test

set -euo pipefail

VPN_HOST="rectella-internationa-wireless-w-tqngtmvdtj.dynamic-m.com"
OPENCONNECT="/usr/bin/openconnect"
PID_FILE="/tmp/rectella-vpn.pid"

# Known Rectella internal IP for health checks (your VPN-assigned address).
HEALTH_IP="172.18.251.117"

# Managed /etc/hosts entries for Rectella hostnames.
# Mullvad DNS leak prevention blocks port 53 from systemd-resolved to
# Rectella's DNS servers, so we fall back to /etc/hosts.
HOSTS_MARKER="rectella-vpn"
RECTELLA_HOSTS=(
  "192.168.3.150  RIL-APP01 RIL-APP01.rectella.com"
  "192.168.3.151  RIL-DB01 RIL-DB01.rectella.com"
)

fix_dns() {
  # vpnc-script sets domains as search-only (no ~ prefix).
  # systemd-resolved then sends queries to the default route (Mullvad's ~.)
  # instead of tun0's DNS servers. Fix by adding ~ prefix to make them
  # routing domains, which take priority over Mullvad for matching queries.
  local domains
  domains=$(resolvectl domain tun0 2>/dev/null | sed 's/^.*: //') || return 0
  if [[ -z "$domains" ]]; then
    return 0
  fi

  local routing_domains=""
  for d in $domains; do
    if [[ $d == ~* ]]; then
      routing_domains="$routing_domains $d"
    else
      routing_domains="$routing_domains ~$d"
    fi
  done

  sudo resolvectl domain tun0 $routing_domains
  sudo resolvectl default-route tun0 false
  echo "DNS routing fixed (domains:$routing_domains)"
}

clean_hosts() {
  if grep -q "BEGIN $HOSTS_MARKER" /etc/hosts 2>/dev/null; then
    sudo sed -i "/# BEGIN $HOSTS_MARKER/,/# END $HOSTS_MARKER/d" /etc/hosts
  fi
}

fix_hosts() {
  clean_hosts

  local block="# BEGIN $HOSTS_MARKER"
  for entry in "${RECTELLA_HOSTS[@]}"; do
    block="$block
$entry"
  done
  block="$block
# END $HOSTS_MARKER"

  echo "$block" | sudo tee -a /etc/hosts >/dev/null
  echo "/etc/hosts updated (RIL-APP01, RIL-DB01)"
}

vpn_up() {
  if [[ -f "$PID_FILE" ]] && sudo kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "VPN already running (PID $(cat "$PID_FILE"))"
    vpn_test
    return 0
  fi

  # Load VPN credentials from .env
  if [[ -f .env ]]; then
    VPN_USER=$(grep -oP '^VPN_USERNAME=\K.*' .env)
    VPN_PASS=$(grep -oP '^VPN_PASSWORD=\K.*' .env)
  fi

  if [[ -z "${VPN_USER:-}" || -z "${VPN_PASS:-}" ]]; then
    echo "Missing VPN_USERNAME or VPN_PASSWORD in .env"
    exit 1
  fi

  # Launch openconnect excluded from Mullvad tunnel.
  # mullvad-exclude uses cgroups — child processes (via sudo) inherit the exclusion.
  echo "Connecting to Rectella VPN (excluded from Mullvad)..."
  echo "$VPN_PASS" | mullvad-exclude sudo "$OPENCONNECT" \
    --user="$VPN_USER" \
    --passwd-on-stdin \
    --background \
    --pid-file="$PID_FILE" \
    "$VPN_HOST"

  # Wait for tun0 to come up.
  echo -n "Waiting for tunnel..."
  for i in $(seq 1 15); do
    if ip link show tun0 &>/dev/null; then
      echo " up"
      break
    fi
    echo -n "."
    sleep 1
  done

  if ! ip link show tun0 &>/dev/null; then
    echo " failed"
    vpn_down
    exit 1
  fi

  fix_dns
  fix_hosts

  echo ""
  vpn_test
}

vpn_down() {
  echo "Disconnecting Rectella VPN..."

  if [[ -f "$PID_FILE" ]]; then
    sudo kill "$(cat "$PID_FILE")" 2>/dev/null || true
    sudo rm -f "$PID_FILE"
  else
    sudo pkill openconnect 2>/dev/null || true
  fi

  # Wait for tun0 to disappear.
  for i in $(seq 1 5); do
    ip link show tun0 &>/dev/null || break
    sleep 1
  done

  clean_hosts
  echo "VPN disconnected."
}

vpn_status() {
  echo "=== Rectella VPN ==="
  if [[ -f "$PID_FILE" ]] && sudo kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "Connected (PID $(cat "$PID_FILE"))"
    ip route show dev tun0 2>/dev/null | sed 's/^/  /'
  else
    echo "Disconnected"
    rm -f "$PID_FILE" 2>/dev/null || true
  fi

  echo ""
  echo "=== Mullvad ==="
  mullvad status 2>/dev/null || echo "Not installed"
}

vpn_test() {
  echo "=== Connectivity Test ==="
  pass=0
  fail=0

  # 1. Mullvad is connected
  if mullvad status 2>/dev/null | grep -q "Connected"; then
    echo "  PASS  Mullvad connected"
    pass=$((pass + 1))
  else
    echo "  FAIL  Mullvad not connected"
    fail=$((fail + 1))
  fi

  # 2. tun0 exists (VPN tunnel interface)
  if ip link show tun0 &>/dev/null; then
    echo "  PASS  tun0 interface exists"
    pass=$((pass + 1))
  else
    echo "  FAIL  tun0 interface missing"
    fail=$((fail + 1))
  fi

  # 3. Can reach VPN-assigned IP (proves tunnel is passing traffic)
  if ping -c 1 -W 3 "$HEALTH_IP" &>/dev/null; then
    echo "  PASS  Ping $HEALTH_IP (VPN internal)"
    pass=$((pass + 1))
  else
    echo "  FAIL  Ping $HEALTH_IP (VPN internal)"
    fail=$((fail + 1))
  fi

  # 4. External traffic goes through Mullvad (not leaking real IP)
  external_ip=$(curl -4 -s --max-time 5 ifconfig.me 2>/dev/null || echo "")
  mullvad_ip=$(mullvad status 2>/dev/null | grep -oP 'IPv4: \K[0-9.]+' || echo "")
  if [[ -n "$external_ip" && "$external_ip" == "$mullvad_ip" ]]; then
    echo "  PASS  External traffic via Mullvad ($external_ip)"
    pass=$((pass + 1))
  elif [[ -n "$external_ip" ]]; then
    echo "  FAIL  External traffic NOT via Mullvad (got $external_ip, expected $mullvad_ip)"
    fail=$((fail + 1))
  else
    echo "  SKIP  Could not determine external IP"
  fi

  # 5. Rectella subnet routes exist
  rectella_routes=$(ip route show dev tun0 2>/dev/null | wc -l)
  if (( rectella_routes >= 5 )); then
    echo "  PASS  Rectella routes present ($rectella_routes routes via tun0)"
    pass=$((pass + 1))
  else
    echo "  FAIL  Rectella routes missing (only $rectella_routes via tun0)"
    fail=$((fail + 1))
  fi

  # 6. DNS resolves RIL-APP01 via Rectella DNS servers
  if resolvectl query RIL-APP01 &>/dev/null; then
    local resolved_ip
    resolved_ip=$(resolvectl query RIL-APP01 2>/dev/null | grep -oP '\d+\.\d+\.\d+\.\d+' | head -1)
    echo "  PASS  DNS resolves RIL-APP01 ($resolved_ip)"
    pass=$((pass + 1))
  else
    echo "  FAIL  DNS cannot resolve RIL-APP01"
    fail=$((fail + 1))
  fi

  echo ""
  echo "Results: $pass passed, $fail failed"
  (( fail == 0 )) && return 0 || return 1
}

case "${1:-}" in
  up)        vpn_up ;;
  down)      vpn_down ;;
  status)    vpn_status ;;
  test)      vpn_test ;;
  fix-hosts) fix_hosts ;;
  *)
    echo "Usage: ./scripts/vpn.sh up|down|status|test"
    exit 1
    ;;
esac
