# Đề xuất: lane subscription → sing-box cho free tier (2026-11-10)

## 0. Kết luận ngắn

- **Có, dùng list free trên GitHub được** — nhưng không dùng list thô. Phải qua
  một aggregator đã **test thật bằng sing-box**, rồi qua **cổng kiểm tra riêng
  của o2a** (đo IP egress + thử đúng endpoint free tier).
- Repo xử lý đúng vấn đề "list free chất lượng kém" tốt nhất hiện tại:
  **`LeilaoMi/AutoMergePublicNodes-Optimized`** (100+ nguồn, test từng node
  bằng sing-box, xuất sẵn `verified.json` cho sing-box, có health report).
  `snakem982/proxypool` là nguồn phụ lớn và ổn định; `onlyactions/proxy-scanner`
  là phương án thay thế.
- **Đo thực tế trên VPS2 hôm nay**: 12/12 node trong `verified.json` sống và
  vào được `opencode.ai`; 5/5 node đem thử **đúng request free tier** đều trả
  **200 trong 2,5–2,8 s** (không WARP, không key). Đây là lane tốt hơn WARP rõ
  rệt.
- Khác biệt cốt lõi: **mỗi IP là một bucket quota**. WARP dùng chung pool đã
  cạn; list free có nhiều IP độc lập, mỗi IP còn quota riêng.

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
     free. 200 → nhận; 429 → loại (ghi `retry_after`). Chi phí: 1/200 quota
     của node, dưới 0,5%.
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

Kết luận: list free đã lọc **tốt hơn WARP** cho mục tiêu quota, và không cần
tài khoản/key.

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

## 7. Việc cần làm để triển khai

1. Viết `deploy/egress-sync/egress-sync.py` (khoảng 250 dòng) theo luồng mục 3.
2. Tạo systemd service + timer 6h; log vào journal.
3. Chạy thử một vòng, so sánh `egress.txt` với 5 node đã đo.
4. Bật `proxyfile`, tắt WARP, reload; theo dõi 30 phút: tỉ lệ 200, p95 latency,
   số 429.
5. Nếu ổn: giữ 24 node; nếu không: hạ 12 node và chỉ dùng làm lane phụ.

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
