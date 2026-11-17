# Bảng chọn 30 đề xuất — o2a v2 (2026-11-12)

Cột **Công**: S ≈ 1–2 h, M ≈ 3–6 h. **Rủi ro** là khả năng gây hồi quy, không phải độ khó.
Bằng chứng viết tắt: R1–R5 = 5 vòng test trong `DE-XUAT-V2-30.md`.

## Giao diện

| # | Việc | Hiện trạng (bằng chứng) | Cách làm | File | Công | Rủi ro | Kỳ vọng |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | Banner lane gate + nút mở khoá | healthz đã trả `lane_gated/lane_until/lane_refusals`; `egress.js` dùng chữ `lane` 0 lần | khối cảnh báo + đồng hồ đếm + `POST /api/egress/lane-reset` (thêm `reset()` cho `laneGate`) | `webui/js/pages/egress.js`, `internal/admin/egress_quota.go`, `internal/warp/pool.go` | S (2 h) | Thấp | Thấy sự cố ngay trên UI, không phải SSH |
| 2 | Cột "200/429 hôm nay" + badge cạn theo dải | quota board đã lưu counters; UI chưa hiện | thêm 3 cột, badge "đã cạn", sắp xếp theo tỉ lệ 429 | `webui/js/pages/egress.js` | S (2 h) | Thấp | Biết dải nào đã đốt hết |
| 3 | Widget TTFT p50/p95 + tỉ lệ 429 | `/metrics` chỉ có counter thô; p95 phải tính ngoài | histogram + tính p50/p95 1 giờ trong `handleMonitor` | `internal/gateway/metrics.go`, `internal/admin/server.go:357`, `webui/js/pages/overview.js` | M (4 h) | Thấp | Nhìn được chất lượng thật, có số để tối ưu |
| 4 | Feed sự kiện ở Overview | journal có dữ liệu, UI không có | ring buffer 200 sự kiện + `GET /api/events` | gateway (mới), `webui/js/pages/overview.js` | M (6 h) | Thấp | Điều tra sự cố ngay trong UI |
| 5 | Cột token_in + badge "reasoning ăn max_tokens" | R5: 2/11 model trả `content` rỗng; token_in 4.597–6.534 | thêm cột và badge ở trang model | `internal/admin/model_quotas.go`, `webui/js/pages/model-quotas.js` | S (2 h) | Thấp | Không chọn nhầm model "hỏng" |
| 6 | Playground SSE trong UI | hiện phải dùng curl | trang gọi `/v1/chat/completions` bằng `fetch` stream, tách reasoning/content | `webui/js/pages/playground.js` (mới) | M (4 h) | Thấp | Thử model ngay trong UI |
| 7 | Nút "Chạy gatecheck" | `cmd/gatecheck` có logic, phải SSH để chạy | endpoint `POST /api/diagnostics/gate` + stream kết quả, badge đỏ khi `DIFF` | `internal/admin/*`, `webui/js/pages/diagnostics.js` | M (3 h) | Thấp | Phát hiện upstream đổi luật trong 1 cú bấm |
| 8 | Lọc log + deep-link request_id + copy curl | log SSE có, không lọc/không permalink | filter client-side + URL `?request=` | `webui/js/pages/diagnostics.js`, `js/logstream.js` | S (2 h) | Thấp | Tìm lỗi nhanh |
| 9 | Responsive + audit i18n | `i18n.js` 1.595 dòng, 2 ngôn ngữ, dễ lệch; bảng egress nhiều cột | sticky cột đầu < 720 px; script so khoá `en`/`vi` | `webui/css/pages.css`, `webui/js/i18n.js`, `webui/scripts/` | S (2 h) | Thấp | Dùng được trên mobile, không thiếu chuỗi |

## Tính năng

| # | Việc | Hiện trạng (bằng chứng) | Cách làm | File | Công | Rủi ro | Kỳ vọng |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 10 | Fallback chuỗi model | R5: 11/11 model free đều 200; 429 hiện là hết lượt | `models.fallback: [...]`, thử model kế trên exit khác | `internal/config/config.go`, `internal/gateway/upstream.go` | M (6 h) | TB: tăng tải upstream, có thể 429 cả 2 model | Tăng tỉ lệ 200 khi một model bị chặn |
| 11 | Affinity có config + metric + chuyển sớm | affinity đã có (`affinity.go`, TTL 30 phút, nối ở `warp.go:208`) nhưng TTL cứng | đưa TTL vào config, đếm hit/miss, đổi dải sớm khi sắp cạn | `internal/gateway/affinity.go`, `warp.go` | S (2 h) | Thấp | Giữ prompt cache, giảm TTFT |
| 12 | Chính sách reasoning | R5: 2/11 model content rỗng | `reasoning.mode = strip \| separate \| passthrough` | `internal/protocol/response.go`, `stream_emitter.go`, `config` | M (4 h) | TB: đổi thứ client thấy | Client luôn có `content` |
| 13 | Buffer max_tokens | R1.2: `max_tokens: 8` → content rỗng, reasoning 8 token | cộng 128 token nội bộ khi `max_tokens ≤ 64`, không đổi `usage` trả về | `internal/gateway/gateway.go`, `internal/protocol/request.go` | S (2 h) | Thấp (chỉ nội bộ) | 100 % câu ngắn có content |
| 14 | Error code + Retry-After | `code: null` (R1.4); v1 429 không header | map 4 loại lỗi + header mốc 00:00 UTC | `internal/protocol/errors.go` | S (2 h) | Thấp | Client backoff đúng, không đập liên tục |
| 15 | Hạn mức theo API key + allowlist | API keys chỉ để xác thực | thêm `daily_tokens`, `allowed_models`, `rpm`; enforce ở middleware | `internal/admin/api_keys.go`, middleware `authenticate` | M (6 h) | TB: cấu hình sai khoá nhầm client | Kiểm soát chia sẻ key |
| 16 | Probe model định kỳ 15 phút | `POST /api/models/probe` phải bấm tay; R5: 11 request ≈ 15 s | scheduler + chỉ expose model 200 gần nhất | `internal/admin/availability.go` + gateway | S (2 h) | Thấp | Danh sách model luôn sống |
| 17 | Quota preflight | R4: `cooling 12` khi `size 6`; R3 có request 19 s vô ích | nếu mọi dải cooling và còn > 5 phút tới reset → 429 ngay + `Retry-After` | `internal/warp/pool.go`, `internal/gateway/upstream.go` | S (2 h) | TB nếu cooling sai → làm #26 trước | Tiết kiệm ~25 s/request khi hết dải |
| 18 | Export CSV/JSON + webhook | báo cáo phải tự ghép JSON; không cảnh báo | `GET /api/egress/export?days=7` + hook Telegram (gate bật/contract đổi/hết dải) | `internal/admin/egress.go`, `internal/telemetry` | M (4 h) | Thấp (cần bot token) | Báo cáo và cảnh báo chủ động |

## Hiệu suất

| # | Việc | Hiện trạng (bằng chứng) | Cách làm | File | Công | Rủi ro | Kỳ vọng |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 19 | Giảm prompt + tools 26 KB → ~8 KB | R5: `prompt_tokens` 4.597–6.534/câu; `opencode_prompt_v2.txt` 8,5 KB + `opencode_tools.json` 17,4 KB | rút mô tả tool, giữ `<env>` + `Working directory` + tool `read`; A/B bằng gatecheck | `internal/gateway/opencode_prompt_v2.txt`, `opencode_tools.json`, `cmd/gatecheck` | M (6 h) | TB: gate có thể từ chối → bắt buộc A/B | TTFT giảm, tải upstream giảm |
| 20 | Giữ client theo đầu ra | `newWarpLease` dựng client mới mỗi lease → mất keepalive dù `MaxIdleConnsPerHost = 512` | cache `http.Client` + `BunTransport` trên exit | `internal/gateway/warp.go` | S (2 h) | Thấp | p95 giảm (đuôi 3,5–4,2 s) |
| 21 | Giảm chi phí admission | ~28 s/exit probe (log 10:42:19→10:42:48) | probe 1/3 exit hoặc khi gate mở; timeout 12 s; cache verdict 5 phút; preload 2 | `internal/warp/pool.go`, `internal/gateway/runtime.go` | S (2 h) | Thấp | Khởi động nhanh hơn, ít device hơn |
| 22 | ETag/304 + gzip cho `/v1/models` | 11.536 byte, không ETag (R1.1) | ETag + `If-None-Match` + gzip | `internal/gateway/gateway.go` (`handleModels`) | S (1 h) | Thấp | Giảm băng thông khi client poll |
| 23 | Metrics mở rộng | chỉ có `o2a_warp_exits{state}` | histogram TTFT theo model, counter rotation theo lý do, gauge lane gate | `internal/gateway/metrics.go`, `internal/warp/pool.go` | S (2 h) | Thấp | Có số để tối ưu tiếp |
| 24 | Fast path cho `stream:false` | non-stream 4,06 s vs stream 2,53 s (R1.2/R1.3) | bỏ máy móc `first_event` khi client không stream | `internal/gateway/first_event.go`, `gateway.go` | S (2 h) | TB: phải đo lại vì mẫu nhỏ | Giảm ~1 s mỗi request non-stream |

## Độ ổn định

| # | Việc | Hiện trạng (bằng chứng) | Cách làm | File | Công | Rủi ro | Kỳ vọng |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 25 | Sửa shutdown admin :8092 | journal 10:42:15 `shutdown_failed`, `context deadline exceeded` | huỷ context log stream trước khi `Shutdown`; rút ngắn deadline | `cmd/opencode2api/main.go:80`, `internal/admin/server.go` | S (2 h) | Thấp | Restart sạch, không mất log |
| 26 | Chuẩn hoá `cooling` | R4: `cooling 12` khi `size 6` | tách "prefix bị loại" khỏi "exit đang nghỉ", janitor dọn hết hạn, cap hiển thị theo size | `internal/warp/pool.go` (`Snapshot`), `health.go` | S (2 h) | Thấp | Chỉ số đúng, cảnh báo đáng tin |
| 27 | Lưu gate + cooling qua restart | restart hiện reset gate → đúc lại device đã cạn (~28 s/device) | ghi file state cạnh `.warp-state.json`, nạp lại khi khởi động | `internal/warp/pool.go`, `internal/gateway/runtime.go` | M (4 h) | TB: state cũ có thể lỗi thời | Không mất quota sau restart |
| 28 | systemd watchdog | `o2a-watchdog` đang inactive | `Restart=always`, `WatchdogSec=60` + `sd_notify` heartbeat | unit `o2a-v2.service`, `cmd/opencode2api/main.go` | S (1 h) | TB: misconfig → restart loop | Tự phục hồi khi treo |
| 29 | Chống bão đăng ký | 8 request song song 8 → pool đúc thêm device | đếm registration/phút, backoff ×2 khi vượt ngưỡng, dọn device cũ trước | `internal/warp/pool.go`, `registrar.go` | M (4 h) | TB: khởi động chậm hơn | Không bị Cloudflare rate limit |
| 30 | Bộ test tích hợp trước release | R1–R5 đang chạy tay | `go test -tags=integration`: gatecheck + 11 model + 8 song song, fail nếu < 80 % 200 | `internal/integration/*_test.go`, `Makefile` | M (6 h) | Thấp | Chặn hồi quy khi nâng binary |

## Gói chọn nhanh

| Gói | Nội dung | Tổng công | Khi nào chọn |
| --- | --- | --- | --- |
| A — Lỗi thật | 25, 26, 13, 14, 17 | ~10 h | Muốn sửa cái đang sai trước (khuyến nghị) |
| B — Nhìn thấy & đo | 23, 3, 1, 2, 5 | ~12 h | Muốn dashboard và UI phản ánh đúng |
| C — Hiệu suất | 20, 21, 19, 22, 24 | ~13 h | Muốn giảm p95 và chi phí mỗi request |
| D — Tính năng | 10, 11, 12, 16, 18 | ~18 h | Muốn tăng tỉ lệ thành công và tiện dùng |
| E — Vận hành chắc | 27, 28, 29, 30, 15 | ~21 h | Chạy dài ngày, nhiều người dùng |

Nếu chỉ chọn 3: **25, 26, 13**. Nếu chọn 5: thêm **17, 14**.

Cần bạn quyết thêm: #18 (bot token Telegram), #15 (chính sách hạn mức key), #19 (được phép đổi prompt — có thể ảnh hưởng cổng upstream, sẽ A/B trước khi deploy).
