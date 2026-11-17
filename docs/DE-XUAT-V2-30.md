# 30 đề xuất cho o2a v2 — sau 5 vòng tự test trên production (2026-11-12)

Tất cả số liệu dưới đây đo trên VPS2 (`161.118.246.9`), binary `6e3ec38f` (v2 + WARP 2 prefix: `2603:c024:4526:9797::/64`, `2001:470:36:965::/64`), trừ khi ghi khác.

## 0. Năm vòng test và kết quả

| Vòng | Nội dung | Kết quả |
| --- | --- | --- |
| R1 | Tính năng: `/v1/models`, chat non-stream, stream SSE, tool call, `/metrics`, `/healthz`, làn v1 :8021 | 12 model (11 free); non-stream 4,06 s; stream 2,53 s / 6 event; tool call trả `read` đúng; metrics 12 dòng; v1 :8021 trả 429 (dải đã cạn) |
| R2 | 8 request stream, song song 4 | **8/8 200**; TTFT p50 **1,58 s**, p95 3,48 s; tổng p50 2,19 s; wall 6 s |
| R3 | Soak 5 phút, 24 request cách 8 s, lấy mẫu healthz + RSS | **24/24 200**; RSS 50–58 MB, không tăng; TTFT p50 ~2,5 s; xấu nhất 19 s (lúc xoay đầu ra) |
| R4 | 8 request song song 8; healthz sau soak; lỗi shutdown | **8/8 200**, wall 4 s, TTFT p50 2,27 s; `cooling: 12` khi `size: 6`; journal có `graceful shutdown failed` ở `:8092` |
| R5 | 1 request mỗi model free | **11/11 200**, 0,96–2,26 s; `prompt_tokens` **4.597–6.534**; 2/11 model trả `content` rỗng vì reasoning ăn hết `max_tokens` |

Baseline để so: TTFT p50 1,6–2,5 s, p95 3,5–4,2 s; 0 % lỗi ở tải ≤ 8; prompt vào upstream ~5,5k token cho một câu 2 chữ.

## 1. Giao diện (9)

| # | Đề xuất | Bằng chứng | Điểm chạm | Công |
| --- | --- | --- | --- | --- |
| 1 | Banner **lane gate** ở trang Egress: trạng thái gated, đồng hồ đếm tới `lane_until`, số lần từ chối, nút "mở khoá ngay" | Backend đã trả `lane_gated/lane_until/lane_refusals`; UI dùng 0 lần chuỗi `lane` | `webui/js/pages/egress.js`, `internal/admin/egress_quota.go`, route mới `POST /api/egress/lane-reset` (phải thêm `reset()` cho `laneGate` — hiện chỉ có `refused`/`admitted`) | S |
| 2 | Bảng egress thêm cột **"lần rút gần nhất"**, **"200/429 hôm nay"**, badge "đã cạn" và sắp xếp theo tỉ lệ 429 | Quota board đã lưu counters theo egress; 3/4 dải mới bị từ chối trong lần đo gatecheck | `webui/js/pages/egress.js` (dữ liệu đã có trong `/api/egress-quota`) | S |
| 3 | Overview: dải chỉ số **TTFT p50/p95 (1 h)**, **tỉ lệ 429**, số đầu ra active/cooling | Hiện chỉ có counters thô; p50/p95 phải tự tính ngoài | histogram ở `internal/gateway/metrics.go` (xem #23), handler `handleMonitor` tại `internal/admin/server.go:357`, `webui/js/pages/overview.js` | M |
| 4 | Overview: **feed sự kiện** 20 dòng gần nhất (xoay đầu ra, gate bật/tắt, contract đổi, 429 theo dải) | Journal có dữ liệu nhưng UI không thấy; điều tra sự cố phải SSH | ring buffer mới trong gateway + `GET /api/events`, `webui/js/pages/overview.js` | M |
| 5 | Trang model: thêm **token_in trung bình** và badge "reasoning chiếm `max_tokens`" cho model hay trả `content` rỗng | R5: 2/11 model trả content 0 ký tự, reasoning 112–113 token; prompt 4,6–6,5k token | `internal/admin/model_quotas.go`, `webui/js/pages/model-quotas.js` | S |
| 6 | **Playground** trong UI: chọn model, nhập prompt, stream SSE, tách `reasoning_content` khỏi `content`, hiện `usage` | Hiện phải dùng curl; R1 cho thấy stream hoạt động ổn | page mới `webui/js/pages/playground.js` + `js/api.js` dùng `fetch` stream | M |
| 7 | Diagnostics: nút **"Chạy gatecheck"**, stream kết quả 9 phép, badge đỏ khi `DIFF` | `cmd/gatecheck` đã có logic; hiện phải SSH để chạy | endpoint `POST /api/diagnostics/gate` gọi lại ma trận, `webui/js/pages/diagnostics.js` | M |
| 8 | Logs: lọc theo `request_id`/status/model, **deep-link** từ lỗi ở Overview, nút "copy curl" | Log stream đã có SSE; không có filter/permalink | `webui/js/pages/diagnostics.js`, `js/logstream.js` | S |
| 9 | **Responsive + i18n audit**: sticky cột đầu bảng egress khi < 720 px; script so khoá `en`/`vi` để phát hiện chuỗi thiếu | `i18n.js` 1.595 dòng, 2 ngôn ngữ, dễ lệch; bảng egress nhiều cột | `webui/css/pages.css`, `webui/js/i18n.js`, script `webui/scripts/i18n-audit.mjs` | S |

## 2. Tính năng (9)

| # | Đề xuất | Bằng chứng | Điểm chạm | Công |
| --- | --- | --- | --- | --- |
| 10 | **Chuỗi fallback model** khi 429/5xx: `models.fallback: ["mimo-v2.6-flash-free", "glm-5.3-flash-free", …]`, thử model kế trên một đầu ra khác | R5: cả 11 model free đều 200 → đổi model rẻ hơn đổi đầu ra | `internal/config/config.go`, vòng thử trong `internal/gateway/upstream.go` | M |
| 11 | **Affinity đã có** (`affinity.go`, TTL 30 phút, nối ở `internal/gateway/warp.go:208`); đề xuất: đưa TTL vào config, thêm metric hit/miss, và **chuyển sớm** khi dải đang giữ sắp cạn thay vì chờ hết TTL | Đo R3: các request trong cùng phiên vẫn đổi đầu ra khi 429 → mất cache | `internal/gateway/affinity.go`, `warp.go` | S |
| 12 | **Chính sách reasoning**: `reasoning.mode = strip \| separate \| passthrough`; `separate` map sang trường `reasoning` kiểu o-series, mặc định `strip` cho client không hiểu | R5: 2/11 model trả content rỗng; client chỉ đọc `content` sẽ thấy câu trả lời trống | `internal/protocol/response.go`, `stream_emitter.go`, `internal/config/config.go` | M |
| 13 | **Buffer max_tokens**: nếu `max_tokens ≤ 64` và model có reasoning, cộng thêm 128 token nội bộ (không đổi `usage` trả về) để `content` không rỗng | R1.2: `max_tokens: 8` → content rỗng, reasoning 8 token; R5 lặp lại ở 2 model | `internal/gateway/gateway.go` (`handleInference`), `internal/protocol/request.go` | S |
| 14 | **Error taxonomy + Retry-After**: điền `error.code` (`quota_exhausted`, `lane_refused`, `contract_moved`, `upstream_error`) và trả `Retry-After` = mốc 00:00 UTC | Hiện `code: null` (R1.4, v1 429); client không biết chờ bao lâu | `internal/protocol/errors.go` | S |
| 15 | **Hạn mức theo API key** + allowlist model: `daily_tokens`, `allowed_models`, `rpm`; enforce trong middleware xác thực | Trang API keys đã có CRUD nhưng chỉ để xác thực | `internal/admin/api_keys.go`, `internal/config` + middleware `authenticate` | M |
| 16 | **Lịch kiểm tra model**: 15 phút/lần gọi probe 11 model, chỉ expose model 200 gần nhất | Đã có `POST /api/models/probe` nhưng phải bấm tay; R5 cho thấy chi phí thấp (11 request ≈ 15 s) | `internal/admin/availability.go` + scheduler trong gateway | S |
| 17 | **Quota preflight**: nếu mọi `/64` đang cooling và còn > 5 phút tới reset thì trả 429 ngay kèm `Retry-After`, không đốt 20–30 s/request | R4: `cooling: 12` với `size: 6`; R3 có request 19 s chỉ để nhận rồi xoay | `internal/warp/pool.go` (`Acquire` trả lỗi sớm), `internal/gateway/upstream.go` | S |
| 18 | **Export + webhook**: `GET /api/egress/export?days=7` (CSV/JSON) và webhook Telegram khi gate bật, contract đổi, hoặc hết dải | Báo cáo hiện phải tự ghép từ JSON; không có cảnh báo chủ động | `internal/admin/egress.go`, hook trong `internal/telemetry` | M |

## 3. Hiệu suất (6)

| # | Đề xuất | Bằng chứng | Điểm chạm | Công |
| --- | --- | --- | --- | --- |
| 19 | **Giảm prompt + tool 26 KB → ~8 KB** (giữ `<env>` + `Working directory` + tool `read`), A/B bằng `cmd/gatecheck` để chắc vẫn 200 | R5: `prompt_tokens` 4.597–6.534 cho câu 2 chữ; `opencode_prompt_v2.txt` 8,5 KB + `opencode_tools.json` 17,4 KB | `internal/gateway/opencode_prompt_v2.txt`, `opencode_tools.json`, `cmd/gatecheck` (thêm biến thể "prompt gọn") | M |
| 20 | **Giữ client theo đầu ra** thay vì tạo mới mỗi lần acquire: `newWarpLease` dựng `http.Client` + `BunTransport` mới cho từng lease, nên kết nối idle bị bỏ dù `MaxIdleConnsPerHost = 512`; cache client trên exit để tái dùng TLS/TCP giữa các request cùng đầu ra | p95 hiện 3,5–4,2 s trong khi p50 1,6–2,5 s; mỗi request mở kết nối mới (TLS 1 lần/request) | `internal/gateway/warp.go` (`newWarpLease`) | S |
| 21 | **Giảm chi phí admission**: probe 1/3 số đầu ra (hoặc chỉ khi gate mở), timeout 12 s, cache verdict 5 phút; preload 2 thay vì 3 | Mỗi đầu ra hiện tốn ~28 s probe (log 10:42:19→10:42:48) + một device Cloudflare | `internal/warp/pool.go` (`admit`), `internal/gateway/runtime.go` | S |
| 22 | **`/v1/models` có ETag/304 + gzip**: 42 model → 12 expose, payload 11,5 KB, client poll định kỳ | R1.1: 11.536 byte, 2,9 ms; tải lặp không cần thiết | `internal/gateway/gateway.go` (`handleModels`) | S |
| 23 | **Metrics bổ sung**: histogram `o2a_ttft_seconds{model}`, counter `o2a_warp_rotations_total{reason}`, gauge `o2a_lane_gated`, counter `o2a_gate_refusals_total` | `/metrics` hiện chỉ có `o2a_warp_exits` theo state; không đo TTFT hay lý do xoay | `internal/gateway/metrics.go`, `internal/warp/pool.go` | S |
| 24 | **Fast path cho `stream:false`**: bỏ máy móc `first_event` (chờ tối đa 240 s) khi client không stream; non-stream đang 4,06 s so với 2,53 s của stream | R1.2 vs R1.3 | `internal/gateway/first_event.go`, `gateway.go` | S |

## 4. Độ ổn định (6)

| # | Đề xuất | Bằng chứng | Điểm chạm | Công |
| --- | --- | --- | --- | --- |
| 25 | **Sửa `graceful shutdown failed`** ở admin `:8092` (SSE log stream không đóng, chờ hết deadline) | Journal 10:42:15: `shutdown_failed`, `address 127.0.0.1:8092`, `context deadline exceeded` | `cmd/opencode2api/main.go:80`, `internal/admin/server.go` (`handleLogStream`) | S |
| 26 | **Chuẩn hoá `cooling`**: hiện `cooling 12` khi `size 6`; tách "prefix bị loại" khỏi "exit đang nghỉ", thêm janitor dọn prefix hết hạn, và giới hạn hiển thị theo size | R4 healthz; R1.6 `cooling 5` | `internal/warp/pool.go` (`Snapshot`), `internal/gateway/health.go` | S |
| 27 | **Lưu trạng thái gate + cooling** vào file state cạnh `.warp-state.json` để restart không mất verdict và không đúc lại device đã cạn | Restart hiện reset gate; R3/R4 cho thấy xoay device tốn ~28 s và quota | `internal/warp/pool.go`, `internal/gateway/runtime.go` | M |
| 28 | **systemd hardening**: `Restart=always`, `WatchdogSec=60` + heartbeat `sd_notify`, bật lại `o2a-watchdog` (đang inactive) | `o2a-watchdog.service` inactive; healthz đã có `ready` để watchdog dựa vào | unit `o2a-v2.service`, `cmd/opencode2api/main.go` | S |
| 29 | **Chống bão đăng ký**: đếm registration/phút trong pool, backoff ×2 khi vượt ngưỡng, dọn device cũ (đã có `unregister`) trước khi đúc mới | R4: 8 request song song 8 → pool phải đúc thêm; mỗi device là tài nguyên Cloudflare có rate limit | `internal/warp/pool.go`, `registrar.go` | M |
| 30 | **Bộ test tích hợp trước release**: `go test -tags=integration` chạy `cmd/gatecheck` + 1 request mỗi model free + 8 request song song, fail nếu < 80 % 200 | R1–R5 chính là bộ test này nhưng chạy tay; contract upstream đổi thì không ai biết | `internal/integration/*_test.go`, `Makefile` | M |

## 5. Nên làm ngay (top 8)

| Thứ tự | Việc | Vì sao trước | Kỳ vọng |
| --- | --- | --- | --- |
| 1 | #25 sửa shutdown | Lỗi thật, mỗi lần restart đều chờ hết deadline và có thể mất log | restart sạch, không mất sự kiện |
| 2 | #26 chuẩn hoá cooling | Chỉ số đang sai (12 > 6) → mọi cảnh báo dựa vào nó đều sai | healthz phản ánh đúng |
| 3 | #13 buffer max_tokens | 2/11 model trả content rỗng, client tưởng model hỏng | content không rỗng ở 100 % case ngắn |
| 4 | #17 quota preflight | Mỗi request khi hết dải tốn 20–30 s vô ích | 429 tức thì, tiết kiệm ~25 s/request |
| 5 | #10 fallback model | 11 model đều sống, đổi model rẻ hơn đổi dải | tăng tỉ lệ 200 khi một model bị chặn |
| 6 | #23 metrics + #3 widget | Không đo được TTFT/429 thì không tối ưu tiếp được | dashboard có p50/p95, tỉ lệ 429 |
| 7 | #1 banner lane gate | Sự cố gate hiện chỉ thấy trong journal | người vận hành thấy ngay trên UI |
| 8 | #19 giảm prompt/tool | 26 KB prompt+tools cho mỗi câu là chi phí lớn nhất còn lại | TTFT giảm, tải upstream giảm |

## 6. Ghi chú

- R3 cho thấy nền tảng đã ổn định ở tải ≤ 8: 0 lỗi, RSS phẳng. Các đề xuất hiệu suất nhắm vào **đuôi phân phối** (p95 3,5–4,2 s) và **chi phí admission**, không phải lỗi.
- Làn v1 (:8021, v1.4.7) hiện trả 429 cho model free — dải của nó đã cạn; v2 là làn đang phục vụ. #26/#27 giúp v1.4.7 không đúc lại device đã cạn sau restart.
- Mọi đề xuất làm được ngay, trừ #18 (cần bot token) và #15 (cần quyết định chính sách key).
