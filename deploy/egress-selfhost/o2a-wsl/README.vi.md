# o2a v2 trên WSL, đi ra bằng Proton ([pVPN](https://github.com/YourDoritos/pVPN) Stealth)

Dùng khi ISP chặn WireGuard UDP trực tiếp (handshake OK nhưng `transfer: 0 B
received`): pVPN dựng **WireGuard-over-TLS** trong WSL, mở SOCKS5 nội bộ, và
o2a v2 chỉ cần trỏ vào SOCKS đó.

```
 o2a-v2 (WSL) ──socks5h──> 192.168.110.88:1080 ──pVPN stealth──> Proton ──> opencode.ai
```

Đã kiểm chứng: tắt `socks5-proton.service` thì gateway trả ngay
`503 {"type":"proxy_error"}`; bật lại thì nhận phản hồi từ OpenCode → toàn bộ
traffic của gateway đi qua tunnel, không lộ IP nhà.

## Cài

```bash
# 1) lấy binary linux + một config.json mẫu từ VPS đang chạy
scp ubuntu@<vps>:/opt/o2a-v2/opencode2api   ./opencode2api
scp ubuntu@<vps>:/opt/o2a-v2/config.json    ./config.vps2.json

# 2) cài trong WSL
wsl -u root -e bash deploy-wsl.sh ./opencode2api ./config.vps2.json
```

Script sẽ:

* đặt binary + config vào `/opt/o2a-wsl`, config `chmod 600` (chứa key);
* ghi `/opt/o2a-wsl/proxies.txt` = `socks5h://192.168.110.88:1080`;
* bỏ `peers` (IP peer VPS không tới được từ PC), `proxies: []`;
* bật `o2a-wsl.service` (listen `0.0.0.0:8077`, web UI `127.0.0.1:8078`).

## Dùng

| Mục | Giá trị |
| --- | --- |
| Base URL từ Windows | `http://192.168.110.88:8077/v1` |
| Base URL trong WSL | `http://127.0.0.1:8077/v1` |
| API key | `server_keys[0]` trong `/opt/o2a-wsl/config.json` |
| Web UI | `http://127.0.0.1:8078` (mở bằng trình duyệt trong WSL, hoặc SSH tunnel) |
| Log | `wsl -u root journalctl -u o2a-wsl -f` |

`127.0.0.1:8077` **không** dùng được từ Windows (WSL2 NAT) — dùng
`192.168.110.88`. Nếu IP WSL đổi sau reboot, cập nhật lại biến trong
`/opt/o2a-wsl/proxies.txt` và base URL phía client.

## Đổi server / bảo trì

```bash
wsl -u root -e /opt/pvpn/pvpnctl status
wsl -u root -e /opt/pvpn/pvpnctl connect JP#164 --protocol stealth
wsl -u root -e systemctl restart pvpn-connect
```

pVPN giữ **một** tunnel nên chỉ có một IP ra tại một thời điểm: đổi server là
đổi IP. Muốn nhiều exit song song thì dùng lớp
[proton-exits](../proton-exits/README.vi.md) trên máy có WireGuard UDP thông
(VPS), rồi cho o2a-v2 ở WSL trỏ `proxyfile` sang các SOCKS đó.

## Giới hạn quota

Đổi IP không thêm quota. Free tier tính theo tài khoản (anonymous dùng chung
`Bearer public`) và cửa sổ theo IP; khi budget cạn thì mọi exit đều 429. Đo
thực tế: 14 quốc gia Proton khác nhau vẫn 429, trong khi instance VPS2 trả 200
chỉ vì nó fallback sang peer VPS1 dùng key khác (log `request served by peer`).
