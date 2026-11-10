#!/usr/bin/env bash
# Probe every configured usque SOCKS5 exit and restart the dead ones.
# Install as a systemd timer; o2a's own health check separately marks the
# proxy transports unhealthy so the gateway stops routing to them meanwhile.
set -u

ports="${USQUE_PORTS:-13001 13002 13003 13004 13005 13006 13007 13008}"
probe_url="${USQUE_PROBE_URL:-https://www.cloudflare.com/cdn-cgi/trace}"
timeout="${USQUE_PROBE_TIMEOUT:-8}"
failures=0

for port in $ports; do
  if ! curl -sS --max-time "$timeout" \
    --proxy "socks5h://127.0.0.1:${port}" "$probe_url" >/dev/null; then
    failures=$((failures + 1))
    logger -t usque-watchdog "port ${port} failed; restarting usque@${port}.service"
    systemctl restart "usque@${port}.service" || true
  fi
done

if [ "$failures" -gt 0 ]; then
  logger -t usque-watchdog "restarted ${failures} exit(s)"
  exit 1
fi
exit 0
