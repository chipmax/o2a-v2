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
| Key upstream 401/403 bị trả nguyên trạng cho client | Đổi thành `502 upstream_credential_rejected` |

## 3. Đề xuất tiếp (theo ưu tiên)

### P0 — Gộp router vào binary Go (bỏ Python, còn 1 tiến trình)

Hiện chuỗi là: client → `router.py:8090` → `o2a-v2:6363` (và VPS1, zen-gate).
Mục tiêu "1 process" chỉ đạt khi router nằm trong Go:

- Thêm mục `upstreams: [{name, base, key, tier, cooldown_seconds}]` vào config.
- Health/cooldown theo upstream (đã có khái niệm trong `internal/gateway/pool.go`).
- Chọn upstream theo tier + round-robin; 5xx/timeout → cooldown upstream đó.
- Lợi ích: bỏ 17MB Python + 1 điểm lỗi; `/healthz` phản ánh cả chuỗi.
- Chi phí: ~200–300 dòng Go + test; không đổi định dạng client.

### P0 — Bảng "ngân sách /64" trong WebUI

- Lưu theo exit: `/64`, họ model, số 429, `reset_at` (từ `Retry-After`).
- Trang **Tổng quan** hiện: exit còn dùng được / đang hết hạn / reset lúc nào.
- Vì sao: hiện phải đọc log mới biết; đây là thông tin vận hành quan trọng nhất.

### P1 — Alias luân chuyển biết quota

`sota`/`sweet` hiện chọn model theo thứ tự cố định. Khi họ `de…` đã hết xô, chuyển
ngay sang họ còn hạn (ví dụ `ic…`, `gl…`) thay vì thử rồi 429.

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
