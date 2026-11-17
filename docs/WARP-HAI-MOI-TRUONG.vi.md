# WARP cho hai môi trường: có IPv6 routed và chỉ có IPv4

Tài liệu này mô tả cách cấu hình WARP trên **VPS có prefix IPv6 routed** và trên **PC/WSL chỉ có IPv4**, cùng lý do tại sao không dùng `stack=ipv4` để tăng số egress.

## 1. Ba chế độ `warp.stack`

| Stack | Địa chỉ trong tunnel | Endpoint handshake | Egress ra `opencode.ai` |
| --- | --- | --- | --- |
| `ipv6` | chỉ IPv6 (`2606:4700:110:…/128`) | ưu tiên IPv6 nếu có `source_ipv6`, nếu không thì IPv4 | `/64` riêng của device — **mỗi device một bucket quota** |
| `dual` | vừa IPv6 vừa IPv4 | **IPv4** khi không có `source_ipv6` ([exit.go:305](../internal/warp/exit.go)) | tuỳ đích: AAAA → `/64` riêng; A → IPv4 Cloudflare dùng chung |
| `ipv4` | chỉ IPv4 | IPv4 | IP Cloudflare dùng chung — **bucket gần như luôn cạn** |

Nguyên tắc quan trọng: **mỗi kết nối TCP ra upstream dùng một họ địa chỉ, một egress**. `dual` không gửi song song hai họ; nó chỉ có sẵn cả hai để chọn theo đích.

## 2. Cấu hình cho VPS có IPv6 routed (VPS2 hiện tại)

```json
"warp": {
  "enabled": true,
  "pool_size": 6,
  "stack": "ipv6",
  "source_ipv6": "2603:c024:4526:9797::/64,2001:470:36:965::/64",
  "preload": 3,
  "attempt_timeout_seconds": 20,
  "pause_after_quota_seconds": 60,
  "rotate_cooldown_seconds": 60
}
```

- `source_ipv6` là các prefix ngoài; [pool.go:557](../internal/warp/pool.go) giữ bit prefix và random các bit thấp bằng `crypto/rand`, nên mỗi đăng ký ra một public `/64` khác nhau.
- Không cần `require_ipv6_egress`: stack `ipv6` luôn ra IPv6.
- Đo thực tế: 7 `/64` khác nhau trong 40 phút, `active=2` tại thời điểm đo.

## 3. Cấu hình cho PC / WSL chỉ có IPv4

```json
"warp": {
  "enabled": true,
  "pool_size": 8,
  "stack": "dual",
  "preload": 4,
  "require_ipv6_egress": true,
  "attempt_timeout_seconds": 20,
  "pause_after_quota_seconds": 60,
  "rotate_cooldown_seconds": 60
}
```

**Không set `source_ipv6`** — nếu set, code sẽ ưu tiên endpoint IPv6 và máy không có IPv6 sẽ dính `udp6 … network is unreachable`.

- Cloudflare WARP có ingress WireGuard IPv4 `162.159.192.0/24` / `162.159.193.0/24`, UDP 2408/500/1701/4500, nên handshake IPv4 vẫn hoạt động ([Cloudflare docs](https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/deployment/firewall/)).
- Device vẫn nhận địa chỉ IPv6 `2606:4700:110:…/128`; khi upstream đi AAAA, egress là public `/64` riêng (`2a09:bac…`).
- `require_ipv6_egress: true` bắt buộc: trong thực đo `pool_size=8` cho `active=2 standby=2` nhưng chỉ có **1 egress IPv6 + 1 egress IPv4 dùng chung** (`104.28.222.73`). Exit IPv4 không tăng quota, cần bị loại.
- Lần đăng ký đầu mất ~50 s; `preload` nên đặt cao hơn mặc định `size/2` để có sẵn bucket trước khi burst.

## 4. Đọc `/healthz` cho đúng

```json
"warp": {
  "stack": "dual", "size": 8, "preload": 4,
  "active": 2, "standby": 2, "egress_ipv6": 1
}
```

- `active` = số exit trong ring, **không** phải số bucket quota.
- `egress_ipv6` = số exit có public egress IPv6; đây là con số cần theo dõi khi mục tiêu là tăng IP.
- `/healthz` vẫn trả 503 `warp_unavailable` trong lúc xoay exit (active tạm thời = 0) — hành vi readiness có chủ đích, request thường vẫn 200 qua fallback khoá.

## 5. Cạm bẫy đã xác nhận bằng đo đạc

1. `stack=ipv4` → tunnel chỉ có IPv4 → egress shared → ít IP hữu ích. **Đừng dùng để tăng quota.**
2. `stack=ipv6` trên host không có IPv6 → 0 exit, log `network is unreachable`.
3. `stack=dual` + đích IPv4 → vẫn rơi về IPv4 shared. Phải đảm bảo upstream phân giải AAAA.
4. Đếm `active` mà không lọc họ egress → đánh giá quá cao số IP thực dụng được.
5. WARP IPv4 dùng chung **không tách riêng được** theo device; Cloudflare có Dedicated Egress IP nhưng là add-on Zero Trust Enterprise, không phù hợp bài toán này.

## 6. Tổng hợp nguồn IP (đo 17/11)

| Nguồn | Số IP |
| --- | --- |
| VPS2: 10 Proton SOCKS | 10 IPv4 unique |
| VPS2: WARP ipv6 + 2 prefix | 7 `/64` trong 40 phút |
| PC: direct ISP | 1 (`171.241.6.97`) |
| PC: sub local (64 outbound, cũ) | 23 sống / 5 IP unique |
| PC: WARP dual (thử) | 2 `/64` riêng, 0 lỗi UDP |
| Sub lane (thử nghiệm, chưa production) | 38 outbound → 16 IP unique |

## 7. Phương án triển khai

1. **Local/WSL trước**: dựng config `dual` như mục 3, `go test ./...`, chạy binary 2 phút, xác nhận `/healthz` có `warp.status=ready`, `egress_ipv6 >= 1`, log không `network is unreachable`. Nếu exit đầu ra IPv4 shared, filter sẽ tự đăng ký lại (tối đa 6 lần).
2. **VPS2 sau**: giữ config `ipv6` + 2 prefix hiện tại; chỉ deploy binary mới. Field mới mặc định (`preload=0`, `require_ipv6_egress=false`) không đổi hành vi. Kiểm tra `/healthz.warp.egress_ipv6` sau restart ~30 s.
3. **Quan sát 24 h**: theo dõi `egress_ipv6` thay vì `active`; nếu `egress_ipv6` luôn nhỏ hơn `active`, đang có exit IPv4 shared hoặc exit chưa verify — không tính vào quota.
4. **Mở rộng**: tăng `preload` dần 4 → 6 → 8 trên PC; trên VPS tăng `pool_size` khi có thêm prefix `/64`. Mỗi lần tăng đo lại số `/64` duy nhất trong log (`ready at`), không tin số exit.
5. **Rollback**: giữ bản backup `opencode2api.bak-20261117d` trên VPS; config cũ vẫn hợp lệ vì field mới là tùy chọn.

Trạng thái deploy hôm 17/11: VPS2 md5 `be3bc4952b7429f12ee9a83ee29f1b45`, `/healthz` `ready=true`, `warp.egress_ipv6=1`; WSL PC cùng binary, `stack=dual`, `require_ipv6_egress=true`, `egress_ipv6=1`, không lỗi UDP.
