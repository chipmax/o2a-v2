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

- `GET /api/egress-quota` + `webui/js/pages/egress.js`, i18n vi/en/it.
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

### P1 — Alias luân chuyển biết quota

`sota`/`sweet` hiện chọn model theo thứ tự cố định. Khi họ `de…` đã hết xô, chuyển
ngay sang họ còn hạn (ví dụ `ic…`, `gl…`) thay vì thử rồi 429.

### P1 — Bảng "ngân sách /64" nâng cấp (đã có bản cơ bản)

- Thêm biểu đồ theo giờ và lịch sử 7 ngày (hiện chỉ giữ trong bộ nhớ, reset khi restart).

### P1 — Lưu usage bằng SQLite (pure Go: `modernc.org/sqlite`)

- Hiện telemetry chỉ nằm trong RAM, restart là mất.
- Bảng: ngày, khoá, model, tier, status, in/out token, latency.
- Dùng cho: biểu đồ 7 ngày trong WebUI, xuất CSV, tính hạn mức ngày chính xác.

### P1 — `/metrics` Prometheus

`requests_total{status,tier,model}`, `quota_429_total{reason}`, `warp_exits{state}`,
`request_duration_seconds` (histogram). Rất rẻ, dùng được với Grafana/Uptime Kuma.

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
