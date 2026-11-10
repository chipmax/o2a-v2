# Kế hoạch nâng cấp o2a-v2 toàn diện (2026-11)

Tổng hợp từ: 22 repo gateway cùng loại + 11 plugin DSH (đọc README/mã, 2026-11-10),
phiên DSH `session-9e547bc4-0870-42fc-a647-b088ca57d599` (48 MB, 13.700 lượt),
log thực tế VPS1/VPS2 và phép đo trần context ngày 2026-11-09.
Trạng thái mới nhất và kế hoạch hiệu chỉnh: xem §8.

## 1. Hiện trạng (đã có)

- 1 binary Go + WebUI nhúng (vi mặc định), hot reload, systemd, ~60 MB RSS.
- Định tuyến: ẩn danh v2 `/inference`, key Zen/Go, **peer fallback** (X-Peer),
  alias xoay (sota/sweet), per-key limit + proxy binding.
- Egress: pool WARP IPv6 /64 (preload 8/16), cooling theo quota, **bảng Ngân sách /64**.
- Giao thức vào: `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/systemone`.
- Giả lập client: Bun TLS, prompt CLI chính thức + `<env>` + skills, session/project id động.
- Mới: `models.context_overrides` (đo được trần thật ~330k, quảng cáo 320k).

## 2. Khảo sát: cái gì đáng học

| Nguồn | Cơ chế | Bài học cho o2a-v2 |
|---|---|---|
| dengyie/opencode2api v1.6.0 | **TetherPool** (egress dính/key từ IP-pool, `rebind_on_wall`), **quota-probe loop** (1 request tối thiểu/key bị 429) | Dò bằng request thật là cách duy nhất khi upstream không có endpoint quota → áp dụng cho context |
| 6Kmfi6HP/opencode2api v0.14.41 | Preamble + tool manifest, egress qua env proxy, labs đo fingerprint/429 | Tài liệu chẩn đoán 429 (quota vs thiếu preamble/TLS) |
| rianilham28/opencode2api (Rust) | IR 4 dialect (chat/responses/anthropic/**gemini**), BoringSSL y hệt Bun, **warm lanes**, `identity` cho JSON | Gemini inbound; tái dùng kết nối khi proxy xoay; bỏ gzip cho endpoint JSON |
| Nystic-Shadow/Opencode2api | Xếp hạng exit theo latency, top-12, failover 20 bước, nhắm region | Chọn exit theo latency thay vì vòng tròn |
| TiaraBasori/OpenCode2API | Native tool passthrough, `cached_tokens` | Chỉ lấy `cached_tokens`; tool shaping hiện tại đã đúng |
| EDDY597/dsh-opencode-gateway | **Registry đo tay** (context, effort, vision, refusal) cho 41 model; quirk `off`, grok/muse chỉ `/responses` | Bảng số đo > tin catalog |
| Paurika1113/dsh-opencode-live | pi-ai đóng gói **snapshot tĩnh** `opencode-go.json` không TTL → context window cũ | Giải thích vì sao client hiện 1M trong khi thật 330k |
| viztor/dsh-opencode-patch | Vá header mạng, session `ses_` hash ổn định, route **theo model** (4 protocol), enrich catalog | Session affinity + route theo protocol khi làm plugin DSH |
| TLEWA/dsh-opencode-go-router | Xoay `x-opencode-session` khi dự đoán cache nguội (context fold/system rewrite), đo TTFT/cache hit | Affinity phải gắn với cache, không chỉ "một id cho vui" |
| a2580vb/dsh-llm-opencode-go, GooDAnDReaDY/dsh-opencode-provider | Provider DSH có `baseURL` cấu hình, discovery, quota panel, per-model context override | Có thể trỏ DSH vào gateway ngay; nền để viết plugin riêng |
| @opencode2dsh/dsh-plugin (đang cài) | Adapter native, gọi thẳng Zen, **không có baseURL** | Đang bỏ qua gateway → mất peers/WARP/quota |
| bowenliang123/dsh-context (4.8k★) | Dashboard/browser context, sự kiện compaction, `/context` | Đối tác tự nhiên: hiển thị số ta quảng cáo và lúc compaction |
| AdamPlatin123/dsh-plugin-radar, DSH-Plugins-Marketplace | Kênh phân phối plugin | Đưa plugin o2a-v2 lên marketplace/topic `dsh-plugin` |

Upstream `jasonxu114514/opencode2api`: không có commit mới từ 2026-11-05; fork đang ngang bản gốc.

## 3. Khoảng trống

1. **Metadata lệch**: catalog 200k, thực 330k; client hard-code 1M; pi-ai snapshot tĩnh.
2. **Quota mở chậm**: key/exit hết quota chỉ được thử lại khi có request thật.
3. **DSH đi đường vòng**: plugin hiện tại gọi thẳng Zen, không qua gateway.
4. **Quan sát**: chưa có `/metrics`, quota board không sống qua restart, trace id chưa xuyên peer.
5. **Hiệu năng**: JSON endpoint còn gzip (chậm khi không tái dùng kết nối), chưa có lane ấm cho proxy xoay.
6. **Phân phối**: chưa deb/Docker/self-update; CI release còn thiếu (workflow scope).
7. **Bảo mật**: WebUI HTTP trần; chưa token cho plugin/admin ngoài loopback.

## 4. Các gói nâng cấp

### P0 — Tự học giới hạn & quota (1–2 ngày, rủi ro thấp)

- `internal/models/limits.go`: registry `{model: {observed_max, rejected_at, source, updated_at}}`,
  lưu `context-limits.json`; hook nhánh success (`usage.prompt_tokens`) và nhánh 400
  `context_length_exceeded`; `/v1/models` chọn `override → learned − đệm → catalog`.
- `POST /api/context-probe {model}` + nút “Đo trần” trong WebUI: binary search với
  `max_tokens=1`, tối đa 6–8 request, chỉ chạy khi idle, backoff, 1 probe in-flight
  (theo đúng pattern quota-probe của dengyie).
- **Quota probe**: mỗi egress/key đang cooling được dò lại bằng 1 request tối thiểu
  mỗi `quota_probe_seconds`; thành công → mở cooldown ngay. Persist bảng quota ra
  `quota-state.json`.
- **Phân loại lỗi + phản hồi**: 400 context trả `X-Context-Limit` và số token;
  log tách `context_limit` / `quota_wall` / `fingerprint_refused`.

### P1 — Tích hợp DSH (4–6 ngày)

- Plugin `o2a-v2-dsh` (native `LlmAdapter`, dựa trên contract của
  `@opencode2dsh`/`a2580vb`): cấu hình `baseURL` + `apiKey` trỏ gateway.
  - `listModels` đọc `/v1/models`: `context_window`, `max_output`, reasoning efforts,
    modalities, `native_protocol` → route theo model (chat/responses/messages, Gemini khi có).
  - Nhóm route theo availability (free / region) như `dsh-zen-proxy` (contract DSH không có
    trường "unavailable").
  - Session affinity: `x-opencode-session` ổn định theo hội thoại, `x-opencode-project`
    hash workspace; tôn trọng cache (không xoay id tùy tiện).
  - Panel: quota từ `/api/egress-quota`, nguồn số (catalog/learned/override),
    trạng thái peer + WARP.
- Trước mắt (1 giờ): thử `dsh-llm-opencode-go`/`dsh-opencode-live` với
  `baseURL=http://161.118.246.9:8090/v1` để xác nhận DSH nhận discovery động.
- Đóng gói npm + đưa lên topic `dsh-plugin`/marketplace.

### P2 — Lõi gateway (1–2 tuần)

- **Gemini inbound** `/v1beta/models/{m}:generateContent` (+stream) qua IR.
- **Warm lanes**: 1 client ấm/session sticky cho proxy xoay, rotate khi idle;
  `identity` cho endpoint JSON; đo lại TTFT trước/sau.
- `/metrics` Prometheus + trace id xuyên peer (`X-Request-Id` propagate).
- Persist lịch sử egress/quota (JSONL) + biểu đồ 7 ngày trong WebUI.
- Chọn exit theo latency (top-N) thay vòng tròn; ưu tiên region khi cấu hình.

### P3 — Vận hành & phân phối (≈1 tuần)

- deb + Docker/compose chính thức; self-update có chữ ký.
- HTTPS trước WebUI (Caddy) + IP allowlist/2FA; admin token cho plugin ngoài loopback.
- CI: build/test/release (cần `gh auth refresh -s workflow`).
- Bộ bench định kỳ (TTFT/throughput/cache hit) chạy theo lịch, so sánh trước/sau.

## 5. Tiêu chí nghiệm thu

| Gói | Đo được |
|---|---|
| P0 | Sau 20 request thật, `/v1/models` tự cập nhật số; 0 request thêm khi học thụ động; probe tay ≤ 8 request và không chạy khi đang có traffic; key cooling được mở ≤ 1 chu kỳ probe |
| P1 | DSH hiện đúng context (320k, không phải 200k/1M); đổi trần upstream → DSH thấy số mới ≤ `refreshSeconds`; panel khớp `/api/egress-quota` |
| P2 | `/metrics` scrape OK; p95 TTFT giảm ≥10% ở chế độ proxy xoay; client Gemini chạy được 1 vòng tool-call |
| P3 | Cài bằng 1 lệnh trên Ubuntu sạch; WebUI chỉ HTTPS; release tự động có checksum |

## 6. Rủi ro

- Probe tốn quota (free tier tính theo request) → chỉ chạy idle, giới hạn tần suất, có công tắc.
- Upstream đổi hành vi (trần, fingerprint) → canary + học thụ động, không hard-code.
- Plugin DSH phụ thuộc contract (`0.1.7 … 0.2.x`) → ghim peer range như EDDY597, test 2 bản.
- Chép mã: upstream GPL-3.0, nhiều repo MIT → giữ GPL khi phát hành, ghi nguồn.

## 7. Cần quyết

1. Làm P0 ngay (tự học + probe + quota probe)?
2. P1: viết plugin riêng `o2a-v2-dsh`, hay PR thêm `baseURL` vào `@opencode2dsh`?
3. Có mở HTTPS/IP allowlist cho WebUI ở P3 không, hay giữ SSH tunnel?

## 8. Cập nhật 2026-11-10 — rà soát phiên DSH `session-9e547bc4-0870-42fc-a647-b088ca57d599`

Nguồn: phiên thật 48 MB trong `~/.dsh/sessions/--F-DSH~0020test--/`, 13.700 lượt,
9.500 tool call, 14,7 MB JSONL; đối chiếu mã nguồn `o2a-v2` đang sửa.

### 8.1 Lỗi/xác nhận từ phiên

| Hiện tượng | Nguyên nhân xác nhận | Hướng xử lý |
| --- | --- | --- |
| `This turn failed Request quota exhausted` | Client trỏ gateway local `:8090` (không WARP), không phải VPS1; local hết exit nên 429 lan | Peer fallback + đưa client về endpoint có WARP/key; đã có `peers` |
| `replace -> 429 -> replace`, sau đó `504 upstream timeout` | Pool bị đốt khi mọi exit mới đều 429; retry đi hết pool thay vì dừng sớm | Tôn trọng `Retry-After`, wall fast-fail, chỉ thử lại sau probe ngắn |
| HTTP 200 nhưng body là `{"type":"error"}` quota | Làn v2 trả 200 + error document khi hết bucket; code cũ forward nguyên body | Coi là lỗi, xoay exit, cuối cùng trả 429 thật |
| Prompt lộ `C:\Windows\System32` | Khối `<env>` lấy `os.Getwd()` của tiến trình gateway | Bỏ dòng working directory/workspace/git repo khỏi prompt |
| Client hiện context 1M, thực tế ~330k | Catalog/snapshot tĩnh; client hard-code | `models.context_overrides` + học thụ động từ `usage.prompt_tokens`/lỗi 400; đã có |
| WSL `execvpe(opencode) failed` | Môi trường WSL thiếu binary, không phải gateway | Ghi chú cài đặt; không sửa mã |
| UI/tài liệu còn tiếng Trung | Bản gốc có `zh-CN` | Đã xoá `README.zh-CN.md`, `docs/management.zh-CN.md`; i18n chỉ còn `vi`/`en` |

Xác nhận cơ chế upstream (issue #158 + đo tại chỗ): quota free tính theo **egress /64**
và **họ model 2 ký tự**; cửa sổ ngày UTC; 429 xảy ra trước `authenticate()` nên
`usage.in = 0`; `Retry-After` là số giây tới 00:00 UTC. Vì vậy xoay /64 có ích, còn
xoay trong cùng /64 thì không.

### 8.2 Đã sửa trong cây làm việc (chưa commit)

- **Quota document**: `doAnonymousUpstream` nhận diện 2xx + body quota, ghi
  `errFreeTierQuotaSpent`, `MarkQuotaSpent` cho exit, trả 429 sau khi hết đường.
  Test: `TestAnonymousQuotaDocumentIsRetiredAndReported`.
- **Wall theo họ model**: 2 tín hiệu 429/quota trong 1 phút mở wall 1 phút; request
  sau chỉ thử **một** exit với timeout 8s rồi chuyển tier key/peer, không đi hết pool.
  Test: `TestAnonymousWallFastFailsPerFamily`.
- **Prompt**: bỏ các dòng thư mục làm việc khỏi `<env>`; giữ platform/ngày UTC.
  README + README.vi + test đã cập nhật.
- **Dọn ngôn ngữ**: xoá toàn bộ copy tiếng Trung khỏi WebUI/tài liệu; chỉ còn vi/en.
- **Xác minh**: `gofmt -l internal` sạch; `go test ./...`, `go vet ./...`,
  `go build ./cmd/opencode2api` đều pass (2026-11-10).

### 8.3 Kế hoạch hiệu chỉnh (ưu tiên theo yêu cầu)

#### P0 — Ổn định & đo đạc (1–2 ngày)

Đã có: học context thụ động, lưu `context-limits.json`, bảng quota `/64` qua restart,
peer fallback, `/metrics`, SQLite usage, per-key limit, wall/quota document.

Còn làm:

1. **`POST /api/context-probe {model}`** + nút “Đo trần” trên WebUI: binary search
   `max_tokens=1`, tối đa 8 request, chỉ chạy khi idle, 1 probe in-flight, có công tắc.
2. **Quota probe chủ động**: mỗi exit/key đang cooling được thử 1 request tối thiểu
   mỗi `quota_probe_seconds`; thành công mở cooldown ngay; lưu lịch sử mở/đóng.
3. **SSE guard**: đệm event đầu ~2s trước khi gửi header; stream rỗng/lỗi ngay thì
   retry exit khác, tránh `stream_failed` nửa chừng. Đánh đổi +0,2–2s mỗi stream.
4. **Phân loại lỗi**: header `X-Context-Limit`, log tách `quota_wall` /
   `context_limit` / `fingerprint_refused`; thêm `quota_429_total{reason}` vào metrics.
5. **Trace xuyên peer**: truyền `X-Request-Id` khi fallback; log một id cho cả chuỗi.

**Nghiệm thu P0**: sau 20 request thật `/v1/models` cập nhật số; probe tay ≤ 8 request
và không chạy khi có traffic; exit cooling được mở ≤ 1 chu kỳ probe; không còn body
200 quota lọt ra client; stream lỗi sớm được retry trước khi gửi header.

#### P1 — Tích hợp DSH & giao diện (4–6 ngày)

1. **Plugin/Provider DSH**: ưu tiên cấu hình `baseURL` + `apiKey` trỏ gateway (dùng
   `dsh-llm-opencode-go`/`dsh-opencode-live` trước); nếu contract cho phép thì viết
   `o2a-v2-dsh` native adapter. `listModels` đọc `/v1/models` (context, max_output,
   reasoning, protocol), nhóm availability, session affinity theo `x-opencode-session`,
   panel quota từ `/api/egress-quota`. Đóng gói npm + topic `dsh-plugin`.
2. **UI gọn hơn nữa**: giữ 4 mục; gộp “Mô hình & Key” thành một trang có tab
   (Mô hình / Key / Luân chuyển); thêm “Kiểm tra tất cả” một nút; trạng thái lỗi
   bằng tiếng Việt dễ hiểu; test i18n key parity vi/en; bỏ hash trang cũ.
3. **Quyết định**: plugin riêng hay PR `baseURL` vào `@opencode2dsh` (mặc định: PR
   trước, plugin riêng chỉ khi cần panel/affinity).

#### P2 — Lõi gateway (1–2 tuần)

- Chọn exit theo latency top-N và region thay vòng tròn; warm lane/session sticky
  cho proxy xoay (đo TTFT trước/sau, mục tiêu p95 giảm ≥10%).
- Tự giãn pool khi cạn (`pool_size < 32`), thu hồi khi rảnh; giới hạn tần suất đăng ký.
- Gemini inbound `/v1beta/models/{m}:generateContent` (+stream) qua IR.
- Biểu đồ 7 ngày + xuất CSV từ SQLite; histogram thật thay phân vị xấp xỉ.
- Cân nhắc MASQUE (`Deury08/warp-minimal`) như sidecar, chỉ thay usque nếu thắng
  ít nhất 24h về 429/TTFB; không nhúng QUIC/gVisor vào binary trước khi có số liệu.

#### P3 — Vận hành & phân phối (≈1 tuần)

- Docker/compose distroless + deb; CI build/test/release kèm checksum.
- HTTPS trước WebUI (Caddy) + IP allowlist/2FA; admin token cho plugin ngoài loopback.
- Self-update có chữ ký; bench định kỳ TTFT/throughput/cache hit.

### 8.4 Rủi ro & quyết định còn mở

- Nhận diện 200-quota phụ thuộc hình dạng body; giữ test và log để phát hiện đổi format.
- Wall 1 phút có thể che hồi phục thật; đã giới hạn bằng 1 probe 8s, có thể chỉnh hằng số.
- Probe chủ động tốn quota → mặc định tắt, chỉ chạy idle và có ngân sách ngày.
- Plugin DSH phụ thuộc contract `0.1.7…0.2.x`; ghim peer range và test 2 bản.
- Cần chốt: (a) làm P0 ngay? (b) PR `baseURL` hay plugin riêng? (c) WebUI HTTPS
  hay tiếp tục SSH tunnel?
