#!/usr/bin/env bash
# Deploy o2a v2 inside WSL, egressing through the pVPN Proton SOCKS5 tunnel.
#
# Run as root inside WSL:
#   wsl -u root -e bash deploy-wsl.sh <path-to-opencode2api> <path-to-config.json>
#
# The gateway listens on 0.0.0.0:8077 (WSL side) and its admin UI on
# 127.0.0.1:8078. Windows reaches the gateway at the WSL address
# (e.g. http://192.168.110.88:8077) — 127.0.0.1 is WSL-only under
# mirrored/NAT networking.
set -euo pipefail

BIN_SRC=${1:?path to the linux opencode2api binary}
CFG_SRC=${2:?path to a config.json to base the local config on}
ROOT=/opt/o2a-wsl
SOCKS=${SOCKS:-socks5h://192.168.110.88:1080}
LISTEN=${LISTEN:-0.0.0.0:8077}
WEBUI=${WEBUI:-127.0.0.1:8078}

install -d -m 755 "$ROOT"
install -m 755 "$BIN_SRC" "$ROOT/opencode2api"

python3 - "$CFG_SRC" "$ROOT/config.json" "$SOCKS" "$LISTEN" "$WEBUI" <<'PY'
import json, sys

src, dst, socks, listen, webui = sys.argv[1:6]
cfg = json.load(open(src))
cfg["listen"] = listen
cfg["proxyfile"] = "/opt/o2a-wsl/proxies.txt"
cfg["proxies"] = []
cfg["peers"] = []
cfg.setdefault("webui", {})["listen"] = webui
cfg.setdefault("performance", {})["max_anonymous_exits"] = 1
json.dump(cfg, open(dst, "w"), indent=2)
print("listen", listen, "| proxy", socks)
PY

chmod 600 "$ROOT/config.json"
printf '%s\n' "$SOCKS" > "$ROOT/proxies.txt"
chmod 644 "$ROOT/proxies.txt"

install -m 644 "$(dirname "$0")/o2a-wsl.service" /etc/systemd/system/o2a-wsl.service
systemctl daemon-reload
systemctl enable --now o2a-wsl.service
sleep 5
systemctl is-active o2a-wsl.service
curl -s -o /dev/null -w 'healthz=%{http_code}\n' "http://127.0.0.1:${LISTEN##*:}/healthz"
