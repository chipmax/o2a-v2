# Proton WireGuard → o2a v2 egress layer

Biến mỗi file `.conf` Proton VPN thành **một exit riêng** cho o2a v2: sing-box
dựng WireGuard trong userspace (không cần `ip link`, không cần `ip rule`,
không cần user riêng) và mở một SOCKS5 nội bộ cho từng tunnel. o2a đọc danh
sách này qua `proxyfile` nên rotation/tính quota/backoff vẫn do gateway lo.

```
  o2a-v2 ──socks5h──> 127.0.0.1:23101 ──userspace WG──> Proton #1 ──> opencode.ai
         ──socks5h──> 127.0.0.1:23102 ──userspace WG──> Proton #2 ──> opencode.ai
         ...
```

## 1. Lấy config Proton

```bash
# đã cài protonvpn-wg-confgen (SRP + TOTP)
protonvpn-wg-confgen --list-servers --json > servers.json
protonvpn-wg-confgen --server JP#164 --protocol wireguard --output ./jp164.conf
```

Log mặc định của Proton đã bật WireGuard. Giới hạn: ~18–20 certificate/giờ
(`certificate error (code 2028): Too many certificates generated`). Đổi tài
khoản Proton **không** làm hỏng config đã tải; chỉ khi revoke certificate
trong tài khoản cũ thì file cũ mới chết.

## 2. Sinh lớp exit

```bash
sudo mkdir -p /opt/o2a-v2/egress/proton
sudo cp *.conf /opt/o2a-v2/egress/proton/
sudo chmod 600 /opt/o2a-v2/egress/proton/*.conf

python3 gen-singbox.py \
  --configs /opt/o2a-v2/egress/proton \
  --out-dir /opt/o2a-v2/egress \
  --base-port 23101
# -> proton-singbox.json (N endpoint + N inbound) + proton.txt (N dòng socks5h)
```

`proton.txt` là proxyfile của o2a; thứ tự dòng khớp thứ tự file `.conf`.

## 3. Chạy

```bash
sudo install -m 644 o2a-proton-exits.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now o2a-proton-exits
```

Trỏ gateway vào proxyfile (`/opt/o2a-v2/config.json`):

```json
{
  "proxyfile": "/opt/o2a-v2/egress/proton.txt",
  "proxies": [],
  "performance": { "max_anonymous_exits": 24 }
}
```

Rồi `sudo systemctl restart o2a-v2` (hoặc `POST /api/config/reload`).

## 4. Kiểm chứng

```bash
# từng exit một IP khác nhau
while read -r u; do
  curl -s --max-time 15 --socks5-hostname "${u#socks5h://}" https://api.ipify.org; echo
done < /opt/o2a-v2/egress/proton.txt

# gateway thật sự đi qua Proton: tắt lớp exit thì phải ra 503 proxy_error
sudo systemctl stop o2a-proton-exits
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $KEY" \
  -d '{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}' \
  http://127.0.0.1:8000/v1/chat/completions     # -> 503
sudo systemctl start o2a-proton-exits           # -> 429/200 từ OpenCode
```

Muốn xem exit nào đang phục vụ thì tạm bật `log.level = "info"` trong
`proton-singbox.json` rồi `journalctl -u o2a-proton-exits | grep wireguard`.

## 5. Đổi IP toàn bộ pool mà không cần certificate mới

WireGuard key của Proton **không gắn với server**: một key dùng được cho mọi
server, chỉ cần `PublicKey` + `hostname` của server đích (lấy từ danh sách
server của tài khoản). Vì vậy 24 key sẵn có có thể trỏ sang 24 server khác
bất cứ lúc nào — đổi sạch IP mà không bị giới hạn certificate/giờ.

```bash
cd /opt/o2a-v2/egress

# lần đầu: tạo proton-keys.json từ các .conf đã tải, và snapshot server list
sudo python3 rotate-exits.py --countries AU --dry-run        # tự bootstrap keys
sudo PROTON_PASSWORD='<mật khẩu>' python3 rotate-exits.py \
     --refresh-servers HectorGalvan2947 --countries AU --dry-run

# đổi sang bộ quốc gia mới (24 key -> 24 server, port giữ nguyên 23101..23124)
sudo python3 rotate-exits.py \
  --countries AU,BR,IN,UK,KR,TW,ID,MY,PH,TR,PL,RO,CZ,IT,ES,ZA,FI,NO,IE,PT,GR,HU,BG,HR
```

`proton.txt` không đổi (vẫn 24 dòng `socks5h://127.0.0.1:23101…23124`) nên
**o2a-v2 không cần sửa config**: rotation chỉ là đổi IP phía sau. Kiểm chứng:

```bash
while read -r u; do curl -s --max-time 15 --socks5-hostname "${u#socks5h://}" https://api.ipify.org; echo; done \
  < /opt/o2a-v2/egress/proton.txt
```

Đo thực tế 2026-11-12: xoay 24 exit sang AU/BR/IN/UK/KR/TW/ID/MY/PH/TR/PL/RO/
CZ/IT/ES/ZA/FI/NO/IE/PT/GR/HU/BG/HR → **24/24 lên, 24 IP mới hoàn toàn**.

Có thể hẹn giờ xoay (ví dụ mỗi 6 giờ) bằng timer systemd gọi chính lệnh trên
với danh sách quốc gia khác nhau. Nên chọn **nhiều quốc gia** hơn là lặp một
quốc gia: nhiều logical server trong cùng nước thường nằm trên cùng một host
vật lý nên IP ra trùng nhau, còn mỗi nước thường là một host khác nhau.

## 6. Giới hạn quota (đo 2026-11-12)

Đổi IP **không** tạo thêm quota. Bằng chứng đo trực tiếp:

| Thử nghiệm | Kết quả |
| --- | --- |
| 24 exit Proton cũ (CA/CH/DE/FR/JP/NL/SE/SG/US) | 429 |
| 14 quốc gia mới qua pVPN ở nhà (VN/LA/MO/HK/TH/KH/MM/BD/BT/BN/SG/NP/MN/LK) | 429 |
| 24 exit **mới tinh** (AU/BR/IN/UK/KR/TW/ID/MY/PH…), IP chưa từng dùng | **429** |
| Cùng key, cùng lúc, qua peer VPS1 (key khác) | **200** |

Vì IP hoàn toàn mới vẫn 429, hạn mức không phải "mỗi IP một bucket" như giả
thuyết cũ: nó đang cạn ở mức **tài khoản/key** (anonymous dùng chung account
`Bearer public`). 24 exit chỉ giúp **phân tán tải, tránh burst limit mỗi IP và
giữ đường ra sạch**, không thêm hạn mức. Muốn có 200 ngay phải dùng key/account
khác, hoặc chờ cửa sổ reset 00:00 UTC (07:00 giờ VN).

Vì vậy với cấu hình hiện tại, request đi qua Proton sẽ nhận 429 và
gateway mới trả 200 nhờ fallback peer; khi peer tắt, client nhận đúng thông
báo hết quota sau ~5 s (1 lượt anonymous + 8 exit key).

