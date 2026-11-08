#!/usr/bin/env bash
# Cài hoặc nâng cấp o2a-v2 trên một VPS Debian/Ubuntu.
#
#   sudo scripts/install.sh            # build (nếu có Go) và cài
#   sudo DEST=/opt/o2a-v2 scripts/install.sh
#
# Cấu hình đang có được giữ nguyên; chỉ tạo config.json từ config.example.json
# khi thư mục đích chưa có.
set -euo pipefail

DEST=${DEST:-/opt/o2a-v2}
UNIT=/etc/systemd/system/o2a-v2.service
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/opencode2api"

if [ "$(id -u)" -ne 0 ]; then
  echo "cần chạy bằng sudo" >&2
  exit 1
fi

if [ ! -x "$BIN" ]; then
  if command -v go >/dev/null 2>&1; then
    echo "== build"
    (cd "$ROOT" && go build -trimpath -ldflags "-s -w -X main.version=v2.0.0-dev" -o opencode2api ./cmd/opencode2api)
  else
    echo "không thấy binary opencode2api và cũng không có go trong PATH" >&2
    exit 1
  fi
fi

if ! id -u o2a >/dev/null 2>&1; then
  echo "== tạo user o2a"
  useradd --system --home "$DEST" --shell /usr/sbin/nologin o2a
fi

echo "== cài vào $DEST"
install -d -o o2a -g o2a -m 755 "$DEST"
install -m 755 -o o2a -g o2a "$BIN" "$DEST/opencode2api"
if [ ! -f "$DEST/config.json" ]; then
  install -m 600 -o o2a -g o2a "$ROOT/config.example.json" "$DEST/config.json"
  echo "!! đã tạo $DEST/config.json từ mẫu — sửa khoá API, mật khẩu WebUI và warp trước khi chạy"
fi

echo "== systemd"
install -m 644 "$ROOT/scripts/o2a-v2.service" "$UNIT"
systemctl daemon-reload
systemctl enable o2a-v2
systemctl restart o2a-v2

echo "== chờ gateway sẵn sàng (WARP đăng ký lần đầu có thể mất ~30s)"
for _ in $(seq 1 90); do
  if curl -fsS -m 2 http://127.0.0.1:6363/healthz >/dev/null 2>&1; then
    curl -fsS http://127.0.0.1:6363/healthz
    echo
    echo "xong. API: http://<ip>:6363/v1 · WebUI: http://<ip>:8081 (hoặc theo webui.listen)"
    exit 0
  fi
  sleep 1
done

echo "gateway chưa trả /healthz sau 90s; xem: journalctl -u o2a-v2 -n 50" >&2
exit 1
