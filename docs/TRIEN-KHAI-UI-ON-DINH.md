# Triển khai: giao diện vận hành + 6 mục ổn định (12/11/2026)

Bản này gộp 9 mục giao diện (nhóm 1) và 6 mục ổn định (nhóm 25–30) trong
`docs/CHON-30-DE-XUAT.md`. Build: Go 1.24, `go build -buildvcs=false ./cmd/opencode2api`.

## Giao diện

| # | Việc | Chỗ thấy |
|---|------|----------|
| 1 | Banner lane bị chặn + nút **Mở khoá ngay** | Tổng quan, Egress & WARP |
| 2 | Badge `429` trên nguồn đã cạn, cột 429/lần cuối sẵn có | Egress & WARP |
| 3 | Thẻ **Tỷ lệ 429** + trạng thái lane | Tổng quan |
| 4 | **Sự kiện gần đây** (20 dòng, tự làm mới 15 s) | Tổng quan |
| 5 | Cột **TB vào/req** trong bảng mô hình | Tổng quan → Mức dùng |
| 6 | (Đã có) tab Thử request thô | Chẩn đoán |
| 7 | Nút **Chạy gatecheck** + kết quả 9 phép | Chẩn đoán → Luồng sự kiện |
| 8 | Lọc log + deep-link `#?request=<id>` | Chẩn đoán → Luồng sự kiện |
| 9 | CSS responsive cho banner/sự kiện; chuỗi mới có bản vi, thiếu khoá thì rơi về tiếng Việt | toàn bộ |

## Ổn định

| # | Việc | Cách làm |
|---|------|----------|
| 25 | Tắt :8092 treo 15 s | `admin.Server.Close()` huỷ context của các SSE trước `Shutdown` |
| 26 | `cooling` khó hiểu (13 > size 6) | `Snapshot` dọn mục hết hạn, thêm `cooling_prefixes`, nhãn "dải /64 đang nghỉ" |
| 27 | Mất trạng thái lane khi restart | `laneGate` lưu `<state>.lane` (0600), nạp lại lúc khởi động |
| 28 | Chết là mất | drop-in systemd: `Restart=always`, `RestartSec=3`, `WatchdogSec=300` + `sd_notify` |
| 29 | Bão đăng ký WARP | Giới hạn 6 registration/phút, chờ thay vì đốt quota |
| 30 | Thiếu test tổng duyệt | `internal/integration` (`-tags=integration`): 11 model free ≥80% 200, 8 luồng song song, health/metrics |

## Endpoint mới (đều cần đăng nhập + CSRF như cũ)

- `POST /api/egress/lane-reset` — xoá pause lane, refill ngay.
- `GET /api/events?limit=50` — sự kiện pool/lane/quota mới nhất.
- `POST /api/diagnostics/gate` — chạy ma trận gatecheck trên **một lease đang sống** (không đăng ký device mới, 9 request nhỏ).
- `GET /api/diagnostics/gate` — trạng thái + report của lần chạy.
- `GET /api/monitor` bổ sung khoá `warp` (snake_case).

## Ghi chú vận hành

- `gatecheck` CLI vẫn dùng như cũ: `go run ./cmd/gatecheck -source <prefix>`; logic nay nằm ở `internal/gatecheck`.
- `WatchdogSec=300` vì lần đăng ký WARP lúc khởi động có thể mất ~60 s; 60 s từng làm systemd giết tiến trình.
- File lane: `/opt/o2a-v2/warp-state.json.lane` (cạnh state WARP). Xoá file = xoá pause.
- Rollback: `sudo cp /opt/o2a-v2/opencode2api.bak-20261112b /opt/o2a-v2/opencode2api && sudo systemctl restart o2a-v2`.

## Kiểm chứng 12/11 23:0x (giờ VN)

- `healthz`: `ready=true`, `warp.cooling=0` sau khi dọn mục hết hạn, 12 model exposed.
- Chat `mimo-v2.6-flash-free`: HTTP 200, 2.6 s (lần đầu sau restart 13.8 s do pool còn đăng ký).
- UI mới phục vụ tại :8092: có `egress-lane`, `overview-ops`, `gate-run`.
- `go test ./...` xanh; `gofmt`/`go vet` sạch (2 file cũ `quotaprobe.go`, `usagehistory.go` vẫn lệch format từ trước).

## Bản vá độ bền 14/11 (rút từ dengyie/opencode2api + afree-proxy)

Bốn việc, mỗi việc có test riêng (`keypool_test.go`, `streamkeepalive_test.go`, `performance_test.go`):

1. **SSE keepalive** — `performance.heartbeat_seconds` (mặc định 15, 0 = tắt). Frame `: keepalive` (OpenAI) hoặc `event: ping` (Anthropic) chỉ được chèn giữa hai dòng hoàn chỉnh: writer giữ lại đuôi dòng dở, nên comment không bao giờ cắt đôi một event.
2. **Idle watchdog in-band** — `performance.idle_timeout_seconds` (mặc định 300, 0 = tắt). Upstream im lặng quá hạn → ghi lỗi in-band `upstream_idle_timeout`, đóng body, và telemetry ghi `outcome=upstream_idle_timeout` thay vì nhầm thành `client_canceled`.
3. **Credential quarantine** — `performance.credential_quarantine_seconds` (mặc định 86400, 0 = tắt). 401/403 của upstream = khoá bị từ chối: khoá ra khỏi vòng xoay cả ngày, cooldown bị ghim theo hạn đó (429 không rút ngắn), probe định kỳ thử lại và chỉ một lần thành công mới gỡ. Cả pool bị quarantine → 503 `key_pool_credential_exhausted` + `Retry-After`, **0 call upstream**.
4. **Quota pre-check** — cả pool đang cooling (và hạn còn > 2 s) → 429 `key_pool_cooling` + `Retry-After`, **0 call upstream**. Kèm sửa lỗi kế toán: phản hồi 200 chứa "quota document" giờ được tính là thất bại (trước đây `MarkSuccess` xoá mất cooldown và khoá bị dùng lại ngay).
5. **Geo-block** — 4xx "not available in your country/region" dừng xoay khoá trong tier và trả 404 `model_not_available_in_region` (không đốt 8 khoá cho cùng một câu trả lời).
6. **Admission gate** — `performance.max_inflight` + `queue_wait_seconds` (mặc định 0 = tắt): request dư chờ tối đa N giây rồi nhận 429 `gateway_busy`; `/healthz` thêm khối `load` khi bật.
7. **Chẩn đoán** — `performance.pprof_enabled` mở `/debug/pprof/` trên cổng WebUI sau đăng nhập (chỉ khi khởi động); `performance.memory_limit_bytes` đặt `debug.SetMemoryLimit`. `/healthz` thêm `keys.credential_quarantined`; mỗi khoá trong bảng tài nguyên có `quarantined_until`.

Kiểm chứng 14/11 22:5x (giờ VN), binary md5 `86b8bc116693f007804d0a5f34b761e4`:

- `healthz`: `ready`, 43 model tổng / 12 exposed, proxies 10/10, warp active 1.
- Chat stream qua router `:8090`: 200, TTFB 1.4 s, 6 event; `/v1/models` 200 (18 model gồm Kilo).
- WebUI `:8092` 200; journal không có `shutdown failed`/`panic`/`stream_idle`/`precheck`.
- Rollback: `sudo cp /opt/o2a-v2/opencode2api.bak-20261114a /opt/o2a-v2/opencode2api && sudo systemctl restart o2a-v2`; config cũ ở `config.json.bak-20261114-perf`.

## Bản vá 15/11 — Route preview + Ready thật

- `POST /api/diagnostics/preview` (đăng nhập + CSRF như mọi API admin): body `{model, protocol}`, trả kế hoạch định tuyến **chỉ đọc** — làn nào `try/fallback/skip`, lý do (wall, breaker, cooldown, quarantine, exit health), không gửi request, không đổi cooldown.
- Tab **Chẩn đoán → Định tuyến** có ô nhập model (datalist từ catalog), chọn protocol và nút Xem trước; bảng kết quả cho thấy từng bước.
- Metric **Trạng thái sẵn sàng** trên Tổng quan lấy từ `/healthz` qua `/api/monitor` (`health.status/ready/issues`), không tự đoán; không còn cảnh UI nói "ready" trong khi `/healthz` từ chối.
- Kiểm chứng 15/11: binary md5 `d784ba69e56ddec7a5bac9b8a4652b97`; `:8000/healthz` 200, `:8092` phục vụ `preview-run` và `m-ready`.

## Bản vá 17/11 — theo dõi upstream v1.4.8 (bỏ Go OAuth)

- **Prefix tool** đổi `clint_` → `mcp_`: upstream xác nhận từ chối literal `clint_` trong tên tool (trả 429 dạng hết quota). Mọi client tool vẫn được rename thành `<prefix><digest>` và đổi lại ở response, nên cặp tên `ReportFindings` + `ScheduleWakeup` của Claude Code vẫn không lọt lên wire. Có test khoá prefix mới.
- **`upstream.prompt_environment`** (xem `docs/CONFIG.vi.md`): ghi đè `working_directory`, `workspace_root`, `platform`, `is_git_repo` trong khối `<env>` và mô tả tool shell. Gateway vẫn chèn đúng **một** cặp `<env>` đầy đủ `Working directory:` — upstream chỉ đọc cặp đầu tiên, một bản stale/duplicate phía trên có thể gây 429.
- **Retry an toàn hơn**: trước mỗi lần thử tier kế tiếp, response của tier trước được đọc tối đa 64 KiB rồi giải phóng socket. Status, `Retry-After` và body lỗi vẫn giữ lại để ánh xạ đúng 429 thay vì bị lấp bởi lỗi transport sau đó.
- Kiểm chứng 17/11 09:1x (giờ VN), binary md5 `72146089c435dd4fc3b97966facedffb`: `gofmt`/`go vet`/`go test ./...`/build xanh; `healthz` 200 `ready=true` sau khi WARP pool sẵn sàng (~33 s); request có tool qua `:8000` → 200 SSE, TTFB 11.4 s (lần này `anon` dính 429 do quota WARP, fallback khoá Zen trả 200 — không phải lỗi prefix); UI `:8092` vẫn có preview/Ready.
- Rollback bản này: `sudo cp /opt/o2a-v2/opencode2api.bak-20261117a /opt/o2a-v2/opencode2api && sudo systemctl restart o2a-v2`. Config không đổi (chỉ thêm khoá tùy chọn `prompt_environment`).

## Bản vá 17/11 (batch 2) — R1/R2/R3

- **R1 env duy nhất**: mọi cặp `<env>…</env>` trong system/instructions do client gửi bị bỏ trước khi prompt CLI chính thức được prepend. Upstream chỉ đọc cặp đầu tiên; một bản stale phía trên bản đầy đủ của gateway từng được đo là trả 429 dạng "hết quota". Áp cho cả ba giao thức (Chat, Responses, Anthropic), có test chặn regression.
- **R2 `models.dead_ids`**: mặc định 2 id Zen đã retire (`longcat-2.5-preview-free` → 410, `nemotron-3-ultra-free` → 400). Bỏ field = dùng mặc định; `[]` = bỏ filter. Lọc ở cả `Route`, `/v1/models` và snapshot, nên cache đĩa cũ không thể đưa model chết lại vào. Có test config + catalog.
- **R3 tool-call audit**: mỗi request có tool renamed phát một dòng INFO `event=tool_call_audit` với `request_id`, `model`, `tool_renames`, `attempts`, `status`, `outcome`; lọt vào `/api/events` dù là INFO. Không log nội dung tool/argument, nên không cần bật `dump_request_bodies`.

Kiểm chứng ổn định 17/11 09:5x (giờ VN), binary md5 `328689e1fd40edeaf151966ddb81671a`:

- WSL (config tạm `/tmp/o2a-wsl-stability.json`, WARP tắt): chạy 60 s, **12/12 `/healthz` = 200**, `ready=true`, `/v1/models` 200, log không warn/error; tiến trình dừng sạch bằng SIGTERM.
- VPS2: **6/6 request có tool → 200 SSE** trong 60 s; `/v1/models` 200 và không còn 2 id chết; 6 dòng `tool_call_audit` xuất hiện; không có `panic`/`fatal`/`shutdown failed`; UI `:8092` 200.
- Một lần `/healthz` trả 503 ở vòng 3 — trùng lúc WARP xoay exit (`active=0` tạm thời, log `Rotating OpenCode WARP egress … HTTP 429`), không phải gateway lỗi: request cùng thời điểm vẫn 200 qua fallback khoá Zen. Đây là hành vi readiness thiết kế, có thể cân nhắc thêm grace window nếu muốn UI không nháy.
- Rollback batch 2: `sudo cp /opt/o2a-v2/opencode2api.bak-20261117b /opt/o2a-v2/opencode2api && sudo systemctl restart o2a-v2`.

## Load test 100 request + WARP trong WSL (17/11 10:3x)

Cùng script `deploy/egress-sync/high-load-test.py`, 3 phase `warm:20:5,load:40:10,stress:40:20` = 100 request, model `deepseek-v4.1-flash-free`.

| Môi trường | warm (c=5) | load (c=10) | stress (c=20) | Sau tải |
| --- | --- | --- | --- | --- |
| **WSL**, `warp.enabled=true`, `stack=ipv4` | 15/20 = 200, 5×429; p50 1.47 s, p95 8.58 s | 40/40 = 200; p50 1.40 s, p95 2.28 s | 40/40 = 200; p50 1.53 s, p95 2.77 s | `healthz` 503 `warp_unavailable`, `panic=0` |
| **VPS2**, WARP ipv6 thật (2 prefix `/64`) | 20/20 = 200; p50 1.93 s, p95 12.21 s | 40/40 = 200; p50 2.38 s, p95 3.06 s | 40/40 = 200; p50 1.58 s, p95 1.83 s, **10.87 rps** | `healthz` 200 `issues: None`, `warp.active=2`, `panic=0`, RSS ~66 MB |

**Vì sao ban đầu tắt WARP trong test WSL:** WSL chỉ có IPv6 ULA/link-local, không có egress IPv6 thật và Cloudflare WARP peer chỉ nghe UDP IPv6 (`write udp6 … network is unreachable`, 44 dòng trong log). Bật `stack=ipv4` vẫn đi ra endpoint IPv6 của WARP nên không cứu được. Gateway vẫn chạy tốt nhờ proxy `direct`; `/healthz` đúng khi chuyển sang 503 sau khi exit giả bị loại. Đây là giới hạn mạng WSL, không phải lỗi gateway — WARP chỉ có thể test thật trên VPS có `/64` routed.

Kết luận: **95/100 (WSL) và 100/100 (VPS2)**, không panic, không rớt service; tăng tải không làm tăng lỗi, latency ổn định ở mức p95 < 3 s (ngoại trừ warm đầu chịu chi phí khởi động catalog/WARP).

## WARP trên host chỉ có IPv4 (17/11 10:5x) — sửa kết luận trước

Kết luận trước ("WSL không chạy được WARP") **sai**: chỉ đúng với `stack=ipv4` hoặc `stack=ipv6`.

- Cloudflare WARP có ingress WireGuard IPv4 `162.159.192.0/24` / `162.159.193.0/24`, UDP 2408/500/1701/4500 ([docs](https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/deployment/firewall/)). Máy chỉ có IPv4 vẫn handshake được.
- [exit.go:305](<F:/DSH test/src/o2a-src/internal/warp/exit.go#L305>) đã **ưu tiên endpoint IPv4 khi không có `source_ipv6`**.
- Cấu hình đúng cho PC/WSL: `warp.enabled=true`, `stack="dual"`, **không设 `source_ipv6`**. `dual` giữ địa chỉ IPv6 trong tunnel; `ipv4` thì không, nên egress rơi về IP Cloudflare dùng chung (`104.28.222.73`).
- Thực đo WSL: `pool_size=4` → 2 exit, **2 `/64` riêng** (`2a09:bac5:d45a:263c::/64`, `2a09:bac5:d45b:2646::/64`), `udp6 unreachable = 0`, `/healthz` 200 `ready=true`. Nâng `pool_size=8` → preload 4, health `active=2 standby=2`, nhưng log chỉ thấy 1 egress IPv6 + 1 egress IPv4 dùng chung → **phải lọc egress theo họ IP/độ duy nhất khi đếm bucket**.
- Xác nhận độc lập: đổi endpoint IPv4 cho 37 WARP node cũ thì 2 node sống, khi gọi đích IPv6 literal cho **2 `/64` khác nhau** (`2a09:bac5:d45f:2646::/64`, `2a09:bac1:7a80:50::/64`).

Đề xuất WARP-first: (1) dùng `dual` + không source prefix trên PC; (2) admission probe phải **chỉ chấp nhận egress IPv6 duy nhất** khi mục tiêu là quota riêng, loại exit ra IPv4 shared; (3) thêm config `warp.preload` (hiện cố định `size/2` ở [pool.go:585](<F:/DSH test/src/o2a-src/internal/warp/pool.go#L585>)) để tạo sẵn nhiều exit thay vì chờ rotation; (4) đưa số `/64` duy nhất vào `/healthz.warp` thay vì chỉ `active`.
