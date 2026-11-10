#!/usr/bin/env bash
# HE 6in4 tunnel for an OCI instance behind 1:1 NAT.
# The VNIC only owns the PRIVATE address, so the tunnel's local endpoint is the
# private IP; OCI rewrites it to the public address on the way out.
#
# usage: he-tunnel.sh up|down|status
set -eu

HE_REMOTE="${HE_REMOTE:-216.218.221.42}"
CLIENT6="${CLIENT6:-2001:470:35:966::2/64}"
ROUTED6="${ROUTED6:-2001:470:36:965::1/64}"
ROUTED_PREFIX="${ROUTED6%::1/64}::/64"
TBL=100

priv_ip() {
  ip -4 -o addr show ens3 | awk '{print $4}' | cut -d/ -f1 | head -1
}

up() {
  local priv
  priv="$(priv_ip)"
  ip tunnel del he-ipv6 2>/dev/null || true
  ip link del he-v6 2>/dev/null || true
  ip tunnel add he-ipv6 mode sit remote "$HE_REMOTE" local "$priv" ttl 255
  ip link set he-ipv6 up
  ip addr add "$CLIENT6" dev he-ipv6
  ip link add he-v6 type dummy
  ip link set he-v6 up
  ip addr add "$ROUTED6" dev he-v6
  ip -6 rule add from "$ROUTED_PREFIX" lookup "$TBL" 2>/dev/null || true
  ip -6 route replace default dev he-ipv6 table "$TBL"
  echo "up: local=$priv remote=$HE_REMOTE client6=$CLIENT6 routed=$ROUTED6"
}

down() {
  ip -6 rule del from "$ROUTED_PREFIX" lookup "$TBL" 2>/dev/null || true
  ip tunnel del he-ipv6 2>/dev/null || true
  ip link del he-v6 2>/dev/null || true
  echo "down"
}

status() {
  ip -6 addr show dev he-ipv6 2>/dev/null | grep -E 'inet6 .*scope global' || echo "he-ipv6: missing"
  ip -6 addr show dev he-v6 2>/dev/null | grep -E 'inet6 .*scope global' || echo "he-v6: missing"
  ip -6 rule show | grep -F "$TBL" || true
  ip -6 route show table "$TBL" 2>/dev/null || true
}

case "${1:-status}" in
  up) up ;;
  down) down ;;
  status) status ;;
  *) echo "usage: $0 up|down|status" >&2; exit 2 ;;
esac
