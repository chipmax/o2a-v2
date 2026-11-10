#!/usr/bin/env bash
# Bring up a Hurricane Electric 6in4 tunnel with a routed /48 and test whether
# each /64 is its own OpenCode free-tier bucket.
#
# Prerequisites (free account, 5 minutes):
#   1. https://tunnelbroker.net -> Create Regular Tunnel with this VPS's IPv4,
#      pick the Singapore POP.
#   2. On the tunnel page click "Create a Routed /48" for the Client IPv6.
#   3. Open the OCI security list for the subnet and allow ingress protocol 41
#      from the HE server IPv4 (the tunnel's own IPv4), and ICMPv6 if you want
#      ping to work.
#
# Usage:
#   sudo ./he-ipv6-tunnel.sh setup <HE_SERVER_V4> <CLIENT_V6> <ROUTED_48>
#   sudo ./he-ipv6-tunnel.sh test  <ROUTED_48> [count]
#   sudo ./he-ipv6-tunnel.sh down
set -euo pipefail

IFACE=he-ipv6
DUMMY=he-v6
FREE='{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":4}'

setup() {
  local server="$1" client_v6="$2" routed="$3"
  local local_v4
  local_v4=$(curl -4 -s -m 8 https://api.ipify.org)
  ip tunnel add "$IFACE" mode sit remote "$server" local "$local_v4" ttl 255 || true
  ip link set "$IFACE" up
  ip -6 addr replace "$client_v6/64" dev "$IFACE"
  ip -6 route replace 2000::/3 dev "$IFACE"
  ip link add "$DUMMY" type dummy 2>/dev/null || true
  ip link set "$DUMMY" up
  ip -6 route replace "$routed" dev "$DUMMY"
  echo "tunnel up: $local_v4 -> $server, client $client_v6, routed $routed"
}

test_ranges() {
  local routed="$1" count="${2:-5}"
  local base="${routed%%::*}"
  for i in $(seq 1 "$count"); do
    local addr
    addr=$(printf '%s::%x::1' "$base" "$i")
    ip -6 addr replace "$addr/64" dev "$DUMMY"
    sleep 1
    local egress status
    egress=$(curl -6 -s -m 8 --interface "$addr" https://api64.ipify.org || true)
    status=$(curl -6 -s -m 25 --interface "$addr" -o /dev/null -w '%{http_code}' \
      -H 'Content-Type: application/json' -d "$FREE" \
      https://opencode.ai/inference/v1/chat/completions || true)
    echo "{\"range\":\"$base::$i::/64\",\"addr\":\"$addr\",\"egress\":\"$egress\",\"free_tier\":\"$status\"}"
  done
}

down() {
  ip link del "$DUMMY" 2>/dev/null || true
  ip link del "$IFACE" 2>/dev/null || true
  echo "tunnel down"
}

case "${1:-}" in
  setup) setup "$2" "$3" "$4" ;;
  test) test_ranges "$2" "${3:-5}" ;;
  down) down ;;
  *) echo "usage: $0 setup <server_v4> <client_v6> <routed_48> | test <routed_48> [count] | down"; exit 2 ;;
esac
