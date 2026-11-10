# Đề xuất: lane subscription → sing-box cho free tier (2026-11-10)

## 0. Kết luận ngắn (đã sửa sau test tải cao)

- **Bước đầu khả quan, nhưng test tải cao thất bại: không nên đưa lane này vào
  vận hành chính.** 5 node đầu tiên trả 200 trong 2,5–2,8 s; nhưng khi dựng
  24 exit và chạy 380 request, **0 request thành công** (toàn 429/503). Kiểm tra
  từng exit: **0/24 còn quota**; 3 exit mới lấy từ `global.json` cũng 429.
- Nguyên nhân: list free là **công khai**, nên các IP này đã bị người khác dùng
  cho chính free tier của OpenCode. Chúng "sống" (vào được web) nhưng **hết
  quota** — đúng kiểu WARP, chỉ khác là nhiều IP hơn.
- Repo lọc chất lượng tốt nhất vẫn là
  **`LeilaoMi/AutoMergePublicNodes-Optimized`** (100+ nguồn, test sing-box thật,
  xuất `verified.json`), nhưng nó chỉ lọc **kết nối**, không lọc **quota của
  OpenCode**. Không có repo nào làm được điều thứ hai.
- Khác biệt cốt lõi: **mỗi IP là một bucket quota**, và bucket của IP công khai
  gần như đã bị tiêu hết. Chỉ IP do mình kiểm soát (VPS, IPv4 phụ, peer) mới
  đáng tin.

## 1. Vì sao "list free chất lượng kém" vẫn dùng được

Chất lượng kém có hai nghĩa khác nhau:

| "Kém" | Ảnh hưởng tới o2a | Cách xử lý |
|---|---|---|
| Node chết, chậm, chập chờn | Request lỗi/treo | Aggregator test thật + health score + loại theo thời gian |
| Node bẩn, cùng IP, hết quota | 429 dù node "sống" | Đo IP thật, dedupe, thử đúng free-tier probe trước khi nhận |
| Node độc hại/ghi log | Rủi ro bảo mật | Chỉ dùng cho traffic free tier; TLS vẫn bảo vệ nội dung; không dùng cho key trả tiền |

List thô không giải quyết được gì. Chuỗi **lọc 3 lớp** mới giải quyết:

```
100+ nguồn GitHub/Telegram
   → [1] aggregator test thật (sing-box/xray)     ~27.000 ứng viên → ~140 verified
   → [2] o2a đo IP + probe free tier               ~140 → 24–30 node nhận
   → [3] runtime quarantine khi 429 (đã có)        node hỏng bị loại tạm thời
```

## 2. Repo nào làm được việc gì

| Repo | Sao | Cập nhật | Vai trò trong pipeline |
|---|---:|---|---|
| **LeilaoMi/AutoMergePublicNodes-Optimized** | 43 | 2026-11-10 | **Nguồn chính**: 100+ subs, test từng node bằng sing-box, xuất `verified.json` / `global.json` đúng định dạng sing-box, kèm điểm và health report |
| **snakem982/proxypool** | 2.260 | 2026-11-10 | Nguồn phụ: cập nhật 4 lần/ngày, sẵn `sing-box-2.json`, nhiều giao thức (ss/vless/hy2/trojan/tuic) |
| **onlyactions/proxy-scanner** | 0 | 2026-11-10 | Thay thế: 180 URL + 90 kênh Telegram, test qua xray/sing-box, xuất subscription xếp hạng (repo mới, ít người dùng) |
| **Toperlock/sing-box-subscribe** | 2.610 | 2026-09-29 | Chuyển Clash/v2ray/URI → cấu hình sing-box khi nguồn không sẵn JSON |
| **sub-store-org/Sub-Store** | 10.867 | 2026-11-10 | Gộp/lọc/đổi tên/script hoá subscription; dùng khi cần trộn nhiều nguồn và loại theo tên/port |
| **tindy2013/subconverter** | 17.111 | 2026-07-09 | Converter cũ, ổn định; ít tính năng lọc hơn Sub-Store |
| **ebouther/opencode-unlimited-proxy** | 62 | 2026-11-04 | Tham khảo kiến trúc: dedupe theo địa chỉ thật, giới hạn ~7 WARP/region, 90 s/rotation |
| **Parsa013Ah/OpenCode-Unlimited-ProxyPool** | 10 | 2026-10-03 | Tham khảo cách test node bằng chính chat Zen trước khi nhận |

Chọn tối thiểu: **LeilaoMi (nguồn) + sing-box-subscribe (nếu cần convert) +
o2a probe (cổng cuối)**. Không cần Sub-Store nếu chỉ dùng một nguồn.

## 3. Kiến trúc đề xuất cho o2a-v2

```
/opt/o2a-v2/
  egress-sync.py          # chạy mỗi 6h (systemd timer)
  egress-state.json       # điểm, IP, lần 429 cuối
  egress.txt              # proxyfile: socks5://127.0.0.1:2200N
  subs/sing-box.json      # do egress-sync sinh ra
```

Luồng:

1. Tải `verified.json` (LeilaoMi) và `sing-box-2.json` (snakem982) — chỉ hai
   URL, không tự scrape hàng trăm nguồn.
2. Tách outbound thật (bỏ `selector`, `url-test`, `direct`), dedupe theo
   `server:port`, rồi theo **IP chính xác**. Mỗi IP chỉ nhận 1 node; tối đa 2–3
   node cùng `/24` (đo hôm nay: 2 node `138.199.35.x` đều 200, nên quota theo
   IP chứ không theo /24).
3. Sinh cấu hình sing-box: mỗi node một **SOCKS inbound** `127.0.0.1:2200N`,
   route cố định inbound → outbound, `final: block`.
4. Chạy `sing-box run`, chờ 3 s.
5. Cổng kiểm tra từng node:
   - `curl --socks5-hostname 127.0.0.1:2200N https://api.ipify.org` → phải có
     IP, và IP chưa có trong danh sách nhận.
   - `curl https://opencode.ai/` → 200/30x.
   - **Probe free tier**: một request `max_tokens: 4` qua node, đúng model
     free. 200 → nhận; 429 → loại (ghi `retry_after`). Cách sạch nhất là dùng
     admin diagnostic `POST /api/debug/inference` với `relay: {name}`: nó pin
     đúng một exit, không đổi cooldown, và trả `http_status`. Bản `egress-sync`
     hiện tại **chưa** làm bước này — vì vậy nó chỉ dùng để thử nghiệm, không
     được bật cho production.
6. Sắp xếp theo latency + tỉ lệ thành công, giữ **24 node** (mặc định), ghi
   `egress.txt`.
7. `POST /api/config/reload` để o2a đọc lại `proxyfile` (không cần restart,
   không mất 60 s đăng ký WARP).
8. Trong lúc chạy: quota board đã ghi 429 theo egress; node nào 429 liên tục
   thì `egress-sync` loại ở vòng sau. Session affinity nên mở rộng cho proxy
   (hiện chỉ WARP) để giữ prompt cache.

Cấu hình o2a tương ứng:

```json
{
  "proxies": [],
  "proxyfile": "/opt/o2a-v2/egress.txt",
  "warp": { "enabled": false },
  "peers": ["http://10.0.0.184:8080"]
}
```

`proxies` để trống nghĩa là free tier chỉ đi qua list; nếu list chết hết, peer
VPS1 là đường dự phòng. Muốn giữ direct làm cuối: thêm `"direct"` vào `proxies`
(nó sẽ được thử trước list — cân nhắc thứ tự).

## 4. Đo thực tế hôm nay (VPS2, 06:46–06:50 UTC)

| Kiểm tra | Kết quả |
|---|---|
| `verified.json` | 142 outbound (86 http, 35 vless, 10 ss, 2 trojan, 1 hy2) |
| 12 node đầu: `api.ipify.org` + `opencode.ai` | **12/12 sống, 12/12 vào opencode 200** |
| 5 node đem thử free tier (`max_tokens:4`, không key, không WARP) | **5/5 trả 200, 2,47–2,81 s** |
| 2 node cùng `/24` (`138.199.35.216`, `138.199.35.209`) | **Cả hai 200** → quota theo IP, không theo /24 |
| So sánh WARP | 429 toàn bộ, 110–180 s/request |
| So sánh direct VPS | 200, 3,0 s |

Kết luận bước đầu: 5 node thử lẻ đều 200, nhanh hơn WARP.

### 4.1. Test tải cao cùng ngày: thất bại

Dựng `egress-sync`, lấy 24 exit unique IP, chạy trên gateway tạm
(`127.0.0.1:18002`, `zen_keys=[]`, `proxies=[]`, `proxyfile` 24 dòng):

| Pha | Số request | Concurrency | 200 | 429 | 503 | p95 |
|---|---:|---:|---:|---:|---:|---:|
| warm | 20 | 4 | 0 | 16 | 4 | 21,9 s |
| load | 120 | 12 | 0 | 120 | 0 | 1,7 s |
| stress | 240 | 24 | 0 | 240 | 0 | 1,2 s |

Kiểm tra từng exit (gateway tạm pin đúng một proxy, hoặc admin diagnostic
`relay`):

| Mẫu | Kết quả |
|---|---|
| 24 exit trong `egress.txt` | **0/24 còn quota** (429 hoặc SOCKS failure 503) |
| 20 exit mới từ `global.json`, khác IP | **0/20**, toàn 429 |
| 3 exit mới, pin riêng từng cái | **3/3 429** |
| Egress IP thật so với `server` khai báo | Khác nhau (ví dụ khai báo `66.163.114.45`, ra `66.163.114.245`) |

Hai bài học:

1. **"Sống" không có nghĩa là "còn quota".** Cổng probe hiện tại chỉ kiểm tra
   `api.ipify.org` + `opencode.ai`, nên vẫn nhận node đã cạn quota.
2. **Dedupe theo `server` là sai**; phải dedupe theo **IP egress đo được**, vì
   proxy chain đổi IP đầu ra. `egress-sync` đã được sửa để dedupe theo IP đo
   được, nhưng điều đó không tạo thêm quota.

Kết luận: lane subscription **không thể làm lane chính** ở thời điểm này. Chỉ
dùng khi có cổng admission kiểm tra đúng free tier và chấp nhận tỉ lệ sống rất
thấp.

## 5. Vì sao không nên tự scrape 100+ nguồn

- Phần lớn nguồn trùng nhau; 27.000 ứng viên chỉ còn ~140 node qua test thật.
- Tự test 27.000 node tốn CPU/RAM và thời gian; VPS2 chỉ 954 MB.
- Aggregator đã làm việc nặng, cập nhật 6h/lần; o2a chỉ cần test 140 node.
- Tự scrape dễ dính nguồn độc hại hoặc nguồn chết hàng loạt.

## 6. Rủi ro cần chấp nhận

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Node chết giữa phiên | Cao | Health score, loại sau 2 lần lỗi, giữ 24 node |
| Node cùng IP/đã hết quota | Trung bình | Đo IP + probe trước khi nhận, dedupe |
| Node ghi log/độc hại | Trung bình | Chỉ dùng cho free tier; TLS bảo vệ nội dung; không dùng cho key trả tiền |
| Vi phạm điều khoản upstream | Cần cân nhắc | Đây là kỹ thuật vận hành; tự đánh giá trước khi dùng |
| List biến mất/đổi format | Thấp | Hai nguồn dự phòng, kiểm tra schema khi tải |

## 7. Việc cần làm nếu vẫn muốn theo hướng này

1. `deploy/egress-sync/egress-sync.py` đã có (fetch, dedupe theo IP đo được,
   render sing-box, probe kết nối, ghi `egress.txt`, unit + timer).
2. **Bổ sung admission probe free tier** bằng admin diagnostic pinned relay;
   chỉ giữ node trả `http_status: 200`. Đây là điều kiện bắt buộc trước khi
   bật cho production.
3. Chạy vòng đầu, đo tỉ lệ 200. Kỳ vọng thực tế rất thấp: **0/24** trong lần
   đo hôm nay.
4. Nếu tỉ lệ 200 > 30% trong 2 vòng liên tiếp: thử làm lane phụ với 8–12 exit,
   theo dõi 24h.
5. Nếu không: **không bật**. Giữ direct + peer VPS1 + Kilo; xem IP phụ/residential
   là cách duy nhất tạo bucket mới đáng tin.

Test tải cao tái lập được bằng `deploy/egress-sync/high-load-test.py`; kết quả
lần đo này: **0/380 request thành công**, nên lane bị loại.

## 8. Nguồn

- <https://github.com/LeilaoMi/AutoMergePublicNodes-Optimized>
- <https://github.com/snakem982/proxypool>
- <https://github.com/onlyactions/proxy-scanner>
- <https://github.com/Toperlock/sing-box-subscribe>
- <https://github.com/sub-store-org/Sub-Store>
- <https://github.com/tindy2013/subconverter>
- <https://github.com/ebouther/opencode-unlimited-proxy>
- <https://github.com/Parsa013Ah/OpenCode-Unlimited-ProxyPool>
- <https://github.com/anomalyco/opencode/issues/60991>
