# Nghiên cứu & đề xuất tiếp theo

Tài liệu này ghi lại những gì đã xác nhận bằng đo đạc/nguồn, những gì bản fork đã sửa,
và danh sách việc nên làm tiếp — xếp theo tỉ lệ lợi ích/chi phí.

## 1. Mô hình quota free tier (đã xác nhận)

Nguồn: issue [#158](https://github.com/jasonxu114514/opencode2api/issues/158) của upstream
(dẫn `packages/console/app/src/routes/zen/util/ipRateLimiter.ts` trên nhánh `v2`) + đo tại chỗ.

- Khoá đếm: `stage:ratelimit:ip:<modelId[0:2]>`. IPv6 bị cắt về **/64**.
  Nghĩa là mỗi **IP exit /64** có một "xô" riêng cho từng **họ model** (2 ký tự đầu).
- Cửa sổ: **ngày lịch UTC**; `Retry-After` trên 429 = số giây tới 00:00 UTC kế tiếp.
- IP mới được **gấp đôi hạn mức** khi `lifetimeCount < dailyLimit * 7`, ưu đãi giảm dần.
- `track()` chạy trên **mọi 200**, kể cả cache hit ⇒ cache hit vẫn tiêu 1 đơn vị.
- 429 được ném **trước** `authenticate()` và trước khi gọi provider ⇒ `usage.in = 0`.
- Hạn mức số nằm trong secret `ZEN_LIMITS`, không công khai.

Hệ quả cho gateway:

1. Xoay exit chỉ có ích vì mỗi exit là một /64 khác nhau; hết cả loạt thì phải chờ 00:00 UTC.
2. `Retry-After` là dữ liệu thật (hàng giờ), không phải nhiễu — phải tôn trọng đầy đủ.
3. Tăng `warp.pool_size` = tăng số xô; VPS1 và VPS2 dùng chung /64 của host nên
   **exit WARP mới là đơn vị quota**, không phải số VPS.
4. Các họ model khác nhau có xô độc lập (tiền tố 2 ký tự), ví dụ `de…`, `ic…`, `gl…`.

## 2. Bản fork này đã sửa gì (liên quan quota)

| Vấn đề | Cách sửa |
| --- | --- |
| `anonymousNodes.MarkFailure` cắt `Retry-After` còn `cooldown×8` (~2 phút) | Tôn trọng đầy đủ, chặn trên 24h; đánh dấu `quotaCooldown` |
| `RestoreProxy` xoá cooldown quota khi proxy "hồi phục" | Bỏ qua node đang `quotaCooldown` |
| Restart làm mất pool WARP, đăng ký lại từ đầu | Lưu `config.json.warp-state.json`, dùng lại; restart <45s thì bỏ qua probe |
| Probe đăng ký cũ ngốn hàng phút lúc khởi động | Cửa sổ probe 4s, mỗi probe 4s, có thể bỏ qua hoàn toàn |
| Free tier trả **HTTP 200 + body lỗi quota** | Nhận diện, coi là lỗi, xoay exit; hết đường trả `429 free_tier_quota_exhausted` |
| Exit trả 200 + body lỗi quota **không bị loại khỏi vòng xoay** | `Pool.ReportQuotaSpent` — nghỉ tới 00:00 UTC, không đăng ký lại vào cùng /64 |
| `429 free_tier_quota_exhausted` luôn ghi `Retry-After: 60` | Trả đúng số giây còn lại tới mốc reset UTC (`quotaResetAfter`) |
| Xoay exit xong là 429 ngay vì exit kế tiếp chưa đăng ký xong | `awaitFreshEgress`: chờ tối đa 4s khi có bản ghi mới đang chạy (`Pool.WaitForActive`) |
| Key upstream 401/403 bị trả nguyên trạng cho client | Đổi thành `502 upstream_credential_rejected` |

## 2b. Số đo ngày 2026-11-08 (phiên omp "Add portable credential vault")

Phiên thật (`sessions\\--C--Windows-System32--\\2026-11-06T13-45-52-749Z_*.jsonl`):

- 620 lượt gọi model, 615 lượt `deepseek-v4.1-flash-free`; 4.12M token vào + 0.65M ra
  trong ngày 06/11 (2.11M + 0.33M ngày 07/11); 95M token đọc từ cache.
- 5 lần `stopReason: length` đều là **job-caption nội bộ của omp** (max_tokens 120, model
  reasoning tiêu hết 120 token vào phần suy luận) — không phải lỗi gateway.
- Không có lỗi 429/quota nào trong phiên; chỉ 1 lượt bị người dùng huỷ (aborted).

Replay 16 lượt kiểu agent (tools `read/grep/bash/write/edit/glob`, context lớn dần tới ~18k):

| Đường | Kết quả | TTFB | Tổng/lượt |
| --- | --- | --- | --- |
| `o2a-v2` (VPS2, pool 16) | 16/16 OK | 1.7–5.7s | 1.9–18.7s |
| `o2a` VPS1 (pool 8) | 8/8 OK | 3.4–7.7s | 2.4–10.6s |

Burst 6 request song song trên pool ấm: 6/6 OK, 1.1–2.5s. Trong 12 phút replay:
17/18 request thành công, 10 leg 429 (mỗi leg ~300ms rồi xoay), `cooling` giữ 9 /64.

Hai giới hạn thật của upstream (đo, không sửa được bằng code):

1. **Đăng ký WARP MỚI bị Cloudflare giới hạn ~28s/exit** ⇒ làm ấm 8 exit mất ~4 phút;
   mỗi lần restart pool về 1 exit và cần từng đó thời gian để ấm lại.
2. **Đăng ký cũ không dùng lại được**: 16 registration lưu trong state file đều
   timeout khi probe sau restart (phiên WireGuard của tiến trình cũ đã mất). Vì vậy
   state file chỉ có giá trị tham khảo, không giữ được pool qua restart.
3. Một /64 chỉ phục vụ được vài request lớn cho họ `de…` (replay 15k-token tiêu 9 /64
   trong ~2 phút) ⇒ năng lực thực tế bị chặn bởi tốc độ đăng ký exit, không phải CPU/RAM
   (RSS ~30MB, CPU gần 0).

## 3. Đã làm (2026-11-08, v2.2–v2.4)

### ✅ Gộp router vào binary Go — một cổng duy nhất

- Config mới `peers: [{name, base_url, key, tier, timeout_seconds, cooldown_seconds, models}]`.
- `internal/gateway/peers.go`: forward nguyên văn khi lane nội bộ thất bại
  (429/hết quota, 5xx, 401/403), tier + round-robin, cooldown biết `Retry-After`,
  soft-fallback khi 400/404 là lỗi “không có model”, header `X-Peer`.
- Peer cũng được thử khi model không có trong catalog nội bộ.
- `/healthz` liệt kê `peers` kèm thời gian cooling.
- **Peer-first theo họ model**: khi free tier báo hết quota họ `de…`, gateway nhớ tới
  mốc reset UTC và gửi request sau tới peer ngay, không xoay exit vô ích.
- Router Python trên VPS2 đã thu về đúng 1 upstream (`vps2-o2a` → `127.0.0.1:6363`);
  các tier zen `ofm-` chết đã bỏ khỏi cấu hình.

### ✅ Trang “Ngân sách /64” trong WebUI

- `GET /api/egress-quota` + `webui/js/pages/egress.js`, i18n vi/en.
- Mỗi dòng: egress /64, họ model, trạng thái (sẵn sàng/đang nghỉ/chưa dùng),
  số request, số lần hết quota, thời gian nghỉ còn lại, mốc reset UTC.

### Số đo sau khi gộp (VPS2, local hết quota họ `de…`)

| Việc | Kết quả |
| --- | --- |
| Request đầu tiên (học trạng thái) | 200 sau 9.3 s, `X-Peer: vps1-o2a` |
| Request tiếp theo | 200 sau **1.0–2.4 s**, `X-Peer: vps1-o2a` |
| Replay 12 lượt agent qua cổng 6363 | **12/12 OK**, TTFB 1.4–5.9 s |
| Chat qua router 8090 (giờ chỉ còn 1 upstream) | 200 sau 1.9 s, `X-Peer: vps1-o2a` |

## 4. Đề xuất tiếp (theo ưu tiên)

### ✅ P1 — Alias luân chuyển biết quota (bản cơ bản, 2026-11-09)

`FailQuota` tạm nghỉ **toàn bộ họ model 2 ký tự** khi một model bị 429 và chuyển
`Current` sang họ còn hạn (ví dụ `ic…`, `gl…`) thay vì thử anh em cùng xô. Phần chọn
theo bảng quota còn lại (số request còn lại từng /64) vẫn là việc tiếp theo.

### P1 — Bảng "ngân sách /64" nâng cấp (đã có bản cơ bản)

- Thêm biểu đồ theo giờ và lịch sử 7 ngày (hiện chỉ giữ trong bộ nhớ, reset khi restart).

### ✅ P1 — Lưu usage bằng SQLite (bản cơ bản, 2026-11-09)

- `internal/telemetry/store.go` dùng `modernc.org/sqlite`, WAL, hàng đợi nền.
- Bảng `usage_requests`: thời điểm, request/session, server key, model, tier, key upstream,
  channel, proxy, attempts, status, latency, outcome, token vào/ra/cache/reasoning/total.
- Bật bằng `storage.usage_sqlite`; lỗi/đầy hàng đợi không chặn request.
- Còn tiếp: biểu đồ 7 ngày trong WebUI, xuất CSV, tính hạn mức ngày chính xác.

### ✅ P1 — `/metrics` Prometheus (bản cơ bản, 2026-11-09)

Đã có `requests_total`, `tokens_total`, `request_duration_seconds`, model/tier/status,
upstream attempts, proxy/WARP health và catalog size. Còn tiếp: `quota_429_total{reason}`
và histogram thật thay cho phân vị xấp xỉ từ summary hiện tại.

### P2 — SSE guard (giảm `stream_failed`)

Đệm ~2s event đầu trước khi gửi header; nếu stream rỗng/lỗi ngay thì retry exit khác.
Đánh đổi: thêm ~0.2–2s cho mọi stream. Nên làm sau khi có số liệu từ SQLite.

### P2 — Tự giãn pool khi cạn

Khi mọi exit đang cooling và `pool_size < 32`: đăng ký thêm vài exit (nền), thu hồi khi rảnh.
Giảm thời gian chờ vào lúc xấu nhất; cần giới hạn để không spam đăng ký Cloudflare.

### P3 — Đóng gói & phát hành

- Dockerfile distroless (image ~15MB), `compose.yaml` mẫu.
- GitHub Actions: build linux/amd64+arm64, kèm checksum và release notes tiếng Việt.
- `systemd` hardening: `ProtectSystem=strict`, `NoNewPrivileges=yes` (WARP cần quyền tạo
  socket, đã kiểm tra chạy được với user thường).

### P3 — Tài liệu client

Hướng dẫn cắm endpoint `http://<ip>:6363/v1` cho: Claude Code, Cursor, Continue,
DSH/opencode, cùng script đổi nhanh (PowerShell/bash).

## 4. Việc cần bạn quyết

1. **OCI ingress 6363**: hiện firewall host đã mở nhưng Security List/NSG của OCI chưa có
   rule TCP 6363 nên từ ngoài vẫn không vào được (đo từ VPS1: public và private đều fail).
   Cần thêm ingress 6363 trong OCI Console (hoặc nói mình dùng 8090 làm cổng công khai).
2. **zen-gate**: có tắt hẳn 2 service `zen-gate`, `zen-gate6` và gỡ các tier zen trong router
   không? (Hiện vẫn chạy nhưng không còn được o2a-v2 dùng; router còn dùng cho model Kilo.)
3. **P0 gộp router**: làm ngay trong binary hay giữ router Python thêm một thời gian?

## 5. Cập nhật 2026-11-09 — health, UI gọn, usque watchdog

### Đã triển khai

- **Health check toàn bộ proxy khi khởi động và mỗi 2 phút**: trước đây chỉ kiểm tra
  proxy đang unhealthy sau một lỗi traffic; usque chết mà SOCKS listener vẫn mở nên
  proxy "healthy" giả. `CheckAll` nay thăm cả 8 exit, ghi `last_checked` và đánh dấu
  unhealthy trước khi request thật mất.
- **`POST /api/proxies/check` + nút “Kiểm tra proxy”** trên trang Egress: chạy một
  lượt kiểm tra thủ công, trả số route đã thử; UI tự refresh monitor và hiện giờ kiểm tra.
- **WebUI 4 mục**: Tổng quan · Mô hình & Key · Egress & WARP · Cài đặt. API Key và
  Chẩn đoán thành thao tác nhanh ở Tổng quan. Bỏ các mục nav trùng/không còn trang.
- **Cache/version**: asset tĩnh vẫn `Cache-Control: no-cache`; thêm header
  `X-O2A-Build` để đối chiếu binary đang chạy.
- **usque watchdog**: `scripts/usque-watchdog.{sh,service,timer}` + `--always-reconnect`
  trong `scripts/usque-run-socks`; probe 8 cổng mỗi 2 phút, restart service chết.
- **WARP nội bộ tạm tắt trên host này**: host WSL không còn route IPv6 toàn cầu; pool
  IPv6 không dựng lại được, IPv4 dựng được nhưng request free tier timeout. Lane free
  đang đi qua 8 exit usque và đã smoke-test 200. Khi có IPv6 thật, bật lại
  `warp.enabled` + `warp.stack=ipv6` để có thêm các xô /64.

### Đề xuất P1 — đánh giá `Deury08/warp-minimal`

Repo tham khảo: <https://github.com/Deury08/warp-minimal> (MIT, Go 1.26+).

Điểm mạnh đáng lấy:

- MASQUE QUIC/UDP 443 (cùng transport 1.1.1.1), không cần root/TUN.
- Pool preload toàn bộ, tự thay exit chết, backoff đăng ký 10–60s.
- Xoay ngay khi 429/403 hoặc lỗi socket; cooldown theo /64 (IPv6) hoặc /32 (IPv4).
- `POST /swap` đổi đúng một exit, `X-Warp-Exit` cho biết exit phục vụ; `/health` JSON.
- uTLS Bun ClientHello và PMTUD cho upload lớn.

Đề xuất tích hợp (không thay usque ngay):

1. Chạy `warp-minimal` như sidecar HTTP proxy nội bộ cạnh usque; thêm vài endpoint
   `http://127.0.0.1:8789` vào `proxies` để so sánh tỷ lệ 429 và TTFB trong 1–2 ngày.
2. Nếu thắng, thay dần usque bằng một pool MASQUE do warp-minimal quản lý; o2a giữ
   health check + quota board, chỉ đổi danh sách proxy.
3. Chỉ nhúng code MASQUE vào binary nếu cần bỏ sidecar; chi phí là quic-go + gVisor
   (~vài MB, thêm bề mặt bảo trì), nên để sau khi số liệu chứng minh lợi ích.

Tiêu chí nghiệm thu: 8/8 exit health, free request 200 trong <10s, không tăng 429
sau 24h, RSS tăng <50MB, và restart không làm mất toàn bộ pool.

### Đo thử 2026-11-09 (sidecar cách ly, cổng 18789)

- Build từ `main` (`go1.26.0`), chạy MASQUE IPv6, pool 2; `/health` báo 2/2 exit
  healthy sau ~8 giây. `curl -x` tới `cloudflare.com/cdn-cgi/trace` trả `warp=on`,
  IPv6 `2a09:bac5:…`, colo HKG.
- Qua o2a tạm (chỉ proxy MASQUE, 3 request `deepseek-v4.1-flash-free`): cả 3 trả
  HTTP 200 nhưng **leg anonymous đều 429**, sau đó fallback Zen key; TTFB 2.6–16.5s.
- Cùng 3 request qua usque đang chạy: 3/3 `anon:200`, TTFB 1.1–1.8s.
- Kết luận: MASQUE chạy được nhưng chưa thay usque cho làn free; giữ sidecar tách
  riêng, không thêm vào `proxies` production. Cần thử lại với exit /64 thật hoặc
  sau 24h trước khi đổi.

## 6. Cập nhật 2026-11-09 — ổn định WebUI và hiển thị egress

- **Model list không hiện khi mở trang**: API hạn mức có thể treo qua tunnel/SSH;
  `load()` giữ cờ `loading` vĩnh viễn nên lần mở sau không thử lại. `api()` nay có
  timeout cứng bằng `Promise.race` + `AbortController` (mặc định 30s, hạn mức 15s,
  các tác vụ dài như kiểm tra proxy/lưu cấu hình có timeout riêng), nên nút và trang
  luôn thoát trạng thái “Đang đọc…” và có thể bấm lại.
- **Monitor 3s chồng request**: `refreshMonitor()` bỏ qua lượt mới khi lượt trước còn
  chạy; tránh dồn request khi tunnel chậm và tránh reset kết nối.
- **Tổng quan không hiện IP WARP**: khi pool vừa restart, `warp.active=0` và
  `warp.egress` rỗng; trang chỉ hiện `—`. Nay lấy thêm `/64` từ `warp.exits` (kể cả
  exit đang cooling) và hiển thị trạng thái “Đang nghỉ / chờ”.
- **i18n**: chỉ còn `vi` và `en`; xoá toàn bộ copy tiếng Trung khỏi WebUI, README và
  tài liệu quản trị. Nav còn 4 mục: Tổng quan · Mô hình & Key · Egress & WARP · Cài đặt.
- **Test quota song song ổn định**: `TestConcurrentQuotaLimit` dùng ngân sách upstream
  60s, chờ đủ 2 request thực sự tới upstream, reset quota toàn cục trước mỗi lượt và
  gom lỗi goroutine qua channel. `go test -race` 5 lượt và `-count=50` đều pass; trước
  đó timeout 5s làm test flake dưới race detector.
- **Phân biệt khoá cổng**: router `8090` dùng mục `keys` riêng trong
  `/opt/o2a-router/config.json`; gateway `6363` dùng `server_keys` của o2a-v2. Gửi khoá
  sai cổng trả `401` dù cả hai đều đúng ở cổng còn lại.
- **Rò socket khi fallback 400/404**: router đọc body lỗi mềm nhưng không đóng response
  upstream; nay đóng trong `finally`. Test 100 request fallback: 200/200,
  `X-Router-Upstream: b`, FD giữ nguyên 4 → 4. Cùng lỗi được đóng ở nhánh transport
  failure và proxy WebUI.
