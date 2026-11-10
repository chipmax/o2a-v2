# Tunnel 6in4 HE tại nhà — WSL, Windows, FortiGate

Tài liệu này ghi lại việc thử dựng tunnel Hurricane Electric **tại máy nhà**
(song song với tunnel đang chạy trên VPS2), các chỗ bị chặn và ba đường đi
khả thi. Ngày đo: 2026-11-11 (giờ VN).

## 1. Kết luận nhanh

| Nơi | Kết quả | Nguyên nhân |
| --- | --- | --- |
| **WSL2** (Debian, kernel `6.18.58.1-microsoft-standard-WSL2`) | **Không được** | `modprobe sit` nạp được module nhưng `ip tunnel add … mode sit` trả `Operation not permitted` — WSL2 chặn tạo thiết bị SIT |
| **Windows 10/11** (`netsh interface ipv6 add v6v4tunnel`) | **Được, cần Administrator** | Stack IPv6 của Windows có sẵn 6in4; lệnh chạy không quyền admin trả `Access is denied` |
| **Thiết bị biên nhà** | **FortiGate FG-100F** | Nhận ra qua TLS cert `CN=FortiGate, O=Fortinet Ltd.` khi gọi `https://192.168.110.1/` |
| **HE tunnelbroker** | Hết quota tạo tunnel trong ngày | `You have already created the maximum number of tunnels today.` |
| **HE /48** | Bị rate-limit 24h | Forum HE: `Error: Maximum /48s requested. Please try again later.` → chờ 24h |

Mạng nhà hiện tại:

```
public IPv4  171.241.6.97        (ICMP từ ngoài vào: chưa kiểm chứng)
PC           192.168.110.88  gw 192.168.110.1  (FortiGate)
public IPv6  không có            (chỉ fe80:: và fd7a:115c:a1e0… của WARP/Tailscale)
Tailscale    100.125.54.5        (đang cài trên Windows)
```

## 2. Điều kiện kỹ thuật của 6in4 (theo HE FAQ)

- Endpoint IPv4 **phải trả lời ICMP echo** (HE ping từ `66.220.2.74`).
- Sau NAT phải **forward IP protocol 41** (protocol, không phải port): DMZ,
  forward theo protocol, hoặc router "hiểu" 6in4.
- CGNAT: gần như không làm được (CGNAT không forward protocol 41).
- IP động vẫn dùng được: HE có `https://ipv4.tunnelbroker.net/nic/update`
  (dyndns2, `hostname=<tunnel id>`), và `ipv4_update.php?tid=…` trên web.
- Mỗi tunnel được **một routed /64**; xin thêm /48 bị giới hạn 24h.
- Tạo tunnel cũng bị giới hạn số lượng/ngày (ta đã chạm trần hôm nay).

Nguồn: [HE IPv6 FAQ](https://ipv6.he.net/certification/faq.php),
[forum /48](https://forums.he.net/index.php?topic=4244.0).

## 3. Đường A — Windows 6in4 (khi FortiGate mở proto 41)

Đã có script: [`he-6in4-windows.ps1`](he-6in4-windows.ps1).

```powershell
# PowerShell với quyền Administrator
.\he-6in4-windows.ps1 -Action setup `
    -LocalIPv4 192.168.110.88 `
    -Client6   2001:470:<xx>:<yyyy>::2 `
    -TunnelId  1044203

.\he-6in4-windows.ps1 -Action status
ping -6 2001:470:<xx>:<yyyy>::1          # peer HE
curl.exe -6 https://api64.ipify.org      # phải ra địa chỉ 2001:470:…
```

`LocalIPv4` là **địa chỉ LAN** của PC (không phải IP public): gói proto 41 đi
ra với source LAN, FortiGate NAT thành `171.241.6.97`; chiều vào FortiGate
phải chuyển proto 41 về `192.168.110.88`.

### Cấu hình FortiGate (cần quyền admin)

Cách 1 — policy "service ALL", không NAT (FortiGate cho các IP protocol khác
TCP/UDP đi qua khi service là ALL):

```
config firewall address
    edit "pc-o2a"
        set subnet 192.168.110.88 255.255.255.255
    next
end
config firewall policy
    edit 0
        set name "wan-to-pc-proto41"
        set srcintf "wan1"            # tên interface WAN thực tế
        set dstintf "internal"        # tên interface LAN thực tế
        set srcaddr "all"
        set dstaddr "pc-o2a"
        set action accept
        set schedule "always"
        set service "ALL"
        set nat disable
    next
end
```

Cách 2 — tạo service theo protocol number rồi policy riêng:

```
config firewall service custom
    edit "IP-41-6in4"
        set protocol IP
        set protocol-number 41
    next
end
```

Cách 3 — đặt PC vào DMZ (mở hết protocol về PC, cần siết host).

Kiểm tra nhanh sau khi mở: từ ngoài (VPS2) chạy
`hping3 -1 …` không đủ; cách chắc chắn là tạo tunnel rồi `ping -6` peer HE.

## 4. Đường B — không cần quyền FortiGate: WireGuard WSL → VPS2

WSL2 **có** WireGuard (đã kiểm tra: `modprobe wireguard` OK, `wg` có sẵn,
`ip link add wgtest type wireguard` OK). VPS2 cũng có module WireGuard.

Mô hình: VPS2 giữ tunnel HE, cắt một dải trong routed /64 (ví dụ
`2001:470:36:965:aaaa::/80`) và route qua WireGuard cho máy nhà. Máy nhà
không cần địa chỉ public, không cần mở gì trên FortiGate (WireGuard đi UDP
ra ngoài và giữ mapping bằng keepalive).

Các bước còn thiếu (chưa chạy được vì phiên Cloud Shell treo):

1. Mở **UDP 51820** trên Default Security List (một rule, làm bằng Cloud
   Shell hoặc console: Networking → VCN → Security Lists → Add Ingress Rule,
   Source `0.0.0.0/0`, Protocol UDP, Destination Port `51820`).
2. Trên VPS2: `apt install wireguard-tools`, bật `net.ipv6.conf.all.forwarding=1`,
   tạo `wg0`:

```
[Interface]
Address = 10.99.0.1/24, fd42:99::1/64
ListenPort = 51820
PrivateKey = <server priv>

[Peer]
PublicKey = dcxIyrG+yhik+1QpqwLxAyswvjf1BVAViNwNG7nazxk=   # WSL home, đã tạo
AllowedIPs = 10.99.0.2/32, fd42:99::2/128, 2001:470:36:965:aaaa::/80
```

3. Trên WSL: `wg set wg0 peer <server pub> endpoint 161.118.246.9:51820
   allowed-ips 0.0.0.0/0,::/0 persistent-keepalive 25`, gán
   `2001:470:36:965:aaaa::2/80`, policy route
   `ip -6 rule add from 2001:470:36:965:aaaa::/80 lookup 200` +
   `ip -6 route add default dev wg0 table 200`.
4. Kiểm tra: `curl -6 --interface 2001:470:36:965:aaaa::2 https://api64.ipify.org`.

Ưu điểm: không đụng FortiGate, không cần admin Windows, chạy được ngay trong
WSL. Nhược điểm: egress vẫn là dải HE của VPS2 (cùng /64 đang bị 429 hôm nay).

## 5. Đường C — thêm tunnel HE để có thêm /64

Mỗi tunnel = một routed /64 riêng. Nhưng:

- HE giới hạn **số tunnel tạo mỗi ngày** (hôm nay đã chạm trần).
- /48 bị **rate-limit 24h**, không phải "không có quyền".
- Có thể dựng tunnel thứ hai trỏ về **VPS2** (không phải nhà) để lấy thêm /64
  mà không phụ thuộc FortiGate — cách này dùng được ngay khi qua ngày.

## 6. Việc cần làm tiếp

| # | Việc | Ai làm |
| --- | --- | --- |
| 1 | Đọc kết quả probe 00:00:20 UTC (`/opt/o2a-v2/egress/he-reset.json`) để biết quota là per-IP hay toàn cục | tự động |
| 2 | Nếu cần đường A: cấp quyền Administrator cho script, và quyền FortiGate để mở proto 41 | bạn |
| 3 | Nếu chọn đường B: mở UDP 51820 trên Security List rồi tôi dựng WireGuard | tôi + 1 rule |
| 4 | Sau 24h: xin /48 cho tunnel VPS2 (nếu cần nhiều /64) | tôi |
