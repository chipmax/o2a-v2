# Pool egress từ subscription sang sing-box (đề xuất)

Tài liệu này trả lời câu hỏi: có nên thay / mở rộng pool WARP hiện tại bằng một
pool SOCKS dựng từ subscription (vmess/vless/trojan/ss/hysteria2), và nếu có thì
chuẩn hoá thế nào cho đúng với cách upstream tính quota.

Nguồn tham khảo chính:

- <https://github.com/trefeon/warp-relay-pool> — pool nhiều egress WARP, keep-cold
  theo 429, một cổng relay cho mỗi node, allowlist đích.
- <https://github.com/trefeon/pi-freeflow> — lớp relay trừu tượng: sticky/spread,
  cooldown, fallback direct khi mọi relay nghỉ.
- <https://sing-box.sagernet.org/configuration/outbound/socks/> — cấu trúc outbound
  SOCKS và các dial field.

## 1. Kết luận ngắn

1. **Không thay** pool WARP hiện tại bằng subscription ngay. WARP cho đúng thứ
   upstream tính: một `/64` mới cho mỗi lần đăng ký, tự động, miễn phí. Subscription
   SOCKS chỉ đáng dùng khi cần **nhiều IP ở nhiều nhà mạng / nhiều quốc gia** hoặc
   khi WARP bị chặn ở một upstream cụ thể.
2. Nếu dùng, hãy dựng **sing-box làm lớp egress duy nhất**, mỗi exit một inbound
   SOCKS riêng, còn gateway chỉ nói chuyện với các cổng loopback đó. Không nhét
   logic subscription vào gateway.
3. Đơn vị quota vẫn là **(egress IP hoặc /64) × họ model × ngày UTC**. Vì vậy mỗi
   exit phải có **danh tính egress đo được** (gọi `https://[2606:4700:4700::1111]/cdn-cgi/trace`
   hoặc `https://api.ipify.org`), không được đặt tên tuỳ ý.
4. Chính sách xoay phải giống `warp-relay-pool`: nghỉ **một** exit khi 429, giữ
   nguyên URL/cổng cho client, cooldown có trần, và chỉ retire khi hết ngày UTC.

## 2. Kiến trúc chuẩn

```
subscription URL / file
        │  (tải định kỳ, xác thực, chuẩn hoá)
        ▼
[egress-sync] ──sinh──► sing-box config.json ──reload──► sing-box
        │                                                │
        │  đọc Clash API (delay, alive)                   ├─ inbound socks 127.0.0.1:21001 → outbound A
        │                                                ├─ inbound socks 127.0.0.1:21002 → outbound B
        ▼                                                └─ ...
[pool state: tag, egress IP, /64, health, cooldown, last 429]
        │
        ▼
gateway/router: chọn exit theo session + quota + health
```

Ba ranh giới rõ ràng:

- **egress-sync**: chỉ tải và chuyển đổi subscription, kiểm tra cấu hình bằng
  `sing-box check`, ghi file 0600, reload có kiểm soát. Không bao giờ chạy lệnh
  từ subscription.
- **sing-box**: chỉ là data plane. Không expose SOCKS ra ngoài loopback; mỗi
  inbound có username/password riêng để gateway không lẫn cổng.
- **gateway/router**: biết quota, session, 429; quyết định xoay hay không.

## 3. Chuyển subscription thành exit

### 3.1. Định dạng đầu vào

Chấp nhận đúng hai dạng, từ chối mọi thứ khác:

- base64 của danh sách URI chia sẻ (`vmess://`, `vless://`, `trojan://`, `ss://`,
  `hysteria2://`);
- Clash YAML chỉ có `proxies:` (bỏ `proxy-groups`, `rules`, script).

Từ chối: HTTP proxy không TLS, URI có `sni` trỏ về IP nội bộ, cổng ngoài
1–65535, tag trùng, quá 200 node, kích thước subscription > 2 MiB.

### 3.2. Ánh xạ sang sing-box

Mỗi node thành một outbound `type` tương ứng (`vmess`, `vless`, `trojan`,
`shadowsocks`, `hysteria2`) và một inbound:

```json
{
  "inbounds": [
    {
      "type": "socks",
      "tag": "in-21001",
      "listen": "127.0.0.1",
      "listen_port": 21001,
      "users": [{"username": "o2a", "password": "<random-32>"}]
    }
  ],
  "outbounds": [
    {"type": "socks", "tag": "placeholder", "server": "127.0.0.1", "server_port": 1}
  ],
  "route": {
    "rules": [
      {"inbound": ["in-21001"], "outbound": "exit-01"}
    ]
  }
}
```

Điểm quan trọng: **một inbound ↔ một outbound**, không dùng `selector`/`urltest`
cho từng request. Selector làm gateway mất khả năng biết request đi qua IP nào,
trong khi quota lại tính theo IP.

Phần outbound thật do egress-sync sinh; ví dụ tối thiểu cho SOCKS upstream:

```json
{
  "type": "socks",
  "tag": "exit-01",
  "server": "203.0.113.10",
  "server_port": 1080,
  "version": "5",
  "username": "u",
  "password": "p"
}
```

### 3.3. DNS và rò rỉ

- Đặt `dns.servers` + `domain_resolver` trên từng outbound để tên miền được giải
  qua chính exit, không qua resolver của VPS.
- Tắt `udp` ở inbound nếu upstream chỉ cần TCP (mặc định của o2a là HTTP/1.1 +
  TLS); bật `udp_over_tcp` chỉ khi provider yêu cầu.
- `route.final` phải là `block`, không phải `direct`: một rule thiếu sẽ làm lộ IP
  VPS thay vì lặng lẽ đi thẳng.

## 4. Danh tính egress và quota

Mỗi exit sau khi dựng phải được đo:

1. `GET https://api.ipify.org?format=json` qua đúng cổng SOCKS → `ip`.
2. Nếu là IPv6, rút `/64`; nếu IPv4, giữ `/32`.
3. Lưu `{tag, port, ip, prefix, first_seen, last_ok, last_429, latency_ms}`.
4. Chỉ đưa vào rotation khi prefix **chưa có** trong pool hoặc đã hết cooldown.
   Trùng prefix là lãng phí một slot quota.

Board `/64` hiện có (`config.json.egress-quota.json`) dùng đúng khoá prefix này,
nên SOCKS exit và WARP exit có thể nằm chung một bảng mà không cần sửa gateway.

## 5. Chính sách xoay và chất lượng

Lấy nguyên tắc từ `warp-relay-pool` và `pi-freeflow`, nhưng thêm phần quota:

| Sự kiện | Hành động | Trần |
| --- | --- | --- |
| 429 có `Retry-After` | nghỉ exit tới mốc đó | không vượt 00:00 UTC |
| 429 không header | nghỉ 30 phút | tối đa 8× backoff |
| 403 lặp ≥ 3 lần / 5 phút | đánh dấu `blocked`, không thử lại 1 giờ | 1 giờ |
| timeout / TLS fail ≥ 2 lần | nghỉ 5 phút | 30 phút |
| 200 nhưng là quota document | retire tới 00:00 UTC | hết ngày |
| 5xx | nghỉ 2 phút | 30 phút |

Chất lượng exit = trung bình trượt của: tỉ lệ 2xx, TTFB, số 429/100 request.
Chỉ chọn exit có quality ≥ ngưỡng; exit mới có ưu tiên trong 10 request đầu để
lấy mẫu. **Không xoay quá 6 exit/phút** để tránh bùng nổ đăng ký / reload.

Session affinity: bám một prefix tối đa 30 phút (đã có trong gateway), nhưng khi
prefix đó bị 429 thì nhả ngay và chọn prefix chất lượng cao nhất còn sống.

## 6. Tích hợp vào o2a-v2

### Giai đoạn 1 — chạy song song, không đụng gateway

- Dựng sing-box + egress-sync trên cùng VPS.
- Trong `config.json` của gateway, khai báo các cổng SOCKS như proxy thường
  (`proxies` + `runtime_proxies`), tắt WARP cho lane tương ứng.
- Đo `/64` bằng script, ghi vào board; so sánh tỉ lệ 429 với WARP trong 1 tuần.

### Giai đoạn 2 — thống nhất pool

- Thêm loại exit `socks` cạnh `warp` trong `internal/warp` (interface
  `Acquire/Report/States` đã đủ): `Acquire` trả lease có `EgressKey()` đọc từ
  pool state; `Report` nhận 429/403 như hiện tại.
- Admin UI dùng chung bảng: cột `source` = `warp` | `socks:<tag>`; nút Xoay/Loại
  gọi `Retire`/`Rotate` sẵn có.
- Không thêm tier mới; chỉ thêm exit source.

### Giai đoạn 3 — subscription tự động

- egress-sync tải subscription mỗi 6 giờ, giữ 2 bản cấu hình (current/next),
  `sing-box check` rồi reload; nếu reload lỗi thì quay lại bản cũ.
- Giới hạn: tối đa 40 exit, 200 node đầu vào, RAM sing-box < 120 MB.
- Cảnh báo khi tỉ lệ 429 toàn pool > 40% trong 15 phút, hoặc khi một prefix
  xuất hiện hai lần.

## 7. So sánh phương án

| Tiêu chí | WARP pool (hiện tại) | Subscription → sing-box | Cloudflare Worker relay |
| --- | --- | --- | --- |
| IP mới mỗi lần xoay | Có, `/64` mới | Tuỳ provider | Không, IP Cloudflare dùng chung |
| Quota theo `/64` | Đúng bản chất upstream | Đúng nếu đo được prefix | Thường không tách được |
| Chi phí | 0 | Giá subscription | Free tier / giới hạn |
| RAM | ~125 MB / 16 exit | ~2–5 MB / exit | 0 |
| Kiểm soát chất lượng IP | Trung bình (WARP) | Cao (chọn provider) | Thấp |
| Rủi ro ToS | Thấp | Phụ thuộc provider | Trung bình |
| Vận hành | Thấp | Trung bình (sync, reload) | Thấp |

Khuyến nghị: giữ WARP làm mặc định; subscription chỉ là **lane dự phòng** khi
WARP bị 429/403 diện rộng hoặc khi cần IP ở quốc gia cụ thể. Với Kilo keyless,
không cần egress riêng (đã đo: gọi trực tiếp từ VPS trả 200), nên đừng đẩy Kilo
qua pool SOCKS.

## 8. Việc cần làm nếu chốt

1. Viết `cmd/egress-sync` (Go, không phụ thuộc Python) + unit test parse URI.
2. Template sing-box + `sing-box check` trước reload.
3. Đo prefix và ghi pool state; hợp nhất với board `/64`.
4. Admin: cột source, nút xoay/loại, cảnh báo trùng prefix.
5. Chạy song song 1 tuần, so sánh 429 và TTFB trước khi đổi mặc định.

## 9. Không nên làm

- Không chạy SOCKS không mật khẩu, kể cả trên loopback.
- Không dùng `urltest`/`selector` cho từng request: mất khả năng quy trách nhiệm
  quota cho một IP.
- Không tự động gửi preamble/tool giả để lách cổng: rủi ro ToS và dễ vỡ khi
  upstream đổi kiểm tra.
- Không dùng chung một subscription cho nhiều VPS nếu provider giới hạn theo
  IP đăng nhập; hãy tách theo máy.
