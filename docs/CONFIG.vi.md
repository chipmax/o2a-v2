# Tham chiếu cấu hình `config.json`

Toàn bộ trường của bản o2a-v2, kèm giá trị mặc định và ghi chú vận hành.
Cấu hình được nạp lại nóng qua `POST /api/config/reload` (WebUI: **Cấu hình → Lưu**);
các trường ghi "cần restart" chỉ đổi sau khi khởi động lại tiến trình.

## 1. Cổng và khoá

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `listen` | string | — | Địa chỉ API OpenAI/Anthropic. Bản này dùng `0.0.0.0:6363`. |
| `server_keys` | string[] | — | Khoá client gửi lên gateway (`Bearer` hoặc `x-api-key`). Tối thiểu 1, tối đa 200. |
| `server_key_metadata` | object | — | Thuộc tính theo fingerprint khoá: `name`, `disabled`, `created_at`, `allow_models`, `max_concurrency`, `daily_requests`. |
| `zen_keys` | string[] | `[]` | Khoá upstream tier Zen (v1 `/zen`). Dùng làm fallback khi free tier hết quota. |
| `go_keys` | string[] | `[]` | Khoá upstream tier Go (`/zen/go`, cần gói trả phí). |
| `anonymous` | bool | `false` | Bật lane miễn phí không cần khoá (`/inference`). |
| `prefer` | string | `go` | Tier được thử trước khi một mô hình phục vụ được ở cả hai tier (`go` hoặc `zen`). |

Ví dụ hạn mức khoá:

```json
"server_key_metadata": {
  "234c0d33b79d72fc": {
    "name": "dsh-pc",
    "allow_models": ["deepseek-v4.1-flash-free"],
    "max_concurrency": 2,
    "daily_requests": 500
  }
}
```

`allow_models` rỗng = mọi mô hình; `max_concurrency`/`daily_requests` = 0 là không giới hạn.
Fingerprint là 16 ký tự hex đầu của SHA-256 khoá, xem ở trang **Khoá API**.

## 2. WARP (lane miễn phí)

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `warp.enabled` | bool | `false` | Bật egress Cloudflare WARP (userspace WireGuard). Bật mà không lên được tunnel thì tiến trình báo lỗi thay vì chạy tiếp. |
| `warp.pool_size` | int | `2` | Số exit WARP giữ nóng, 1–32. Quota free tier tính **theo IP exit**, nên pool lớn hơn = nhiều "xô" quota hơn. 16 là mức đang dùng. |
| `warp.stack` | string | `ipv6` | `ipv6` (khuyến nghị) hoặc `ipv4`. |
| `warp.source_ipv6` | string | `""` | Prefix ngoài (thường /64) để mỗi exit bắt socket từ một địa chỉ khác nhau. Bắt buộc khi `stack=ipv6` muốn xoay IP. |

Đăng ký WARP được lưu ở `config.json.warp-state.json` (0600). Restart trong vòng 45s
sẽ bỏ qua probe (tunnel cũ còn giữ session) và đăng ký mới; restart sau đó sẽ thử
dùng lại, tối đa 4s cho toàn bộ giai đoạn probe.

## 3. Proxy

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `proxies` | string[] | `["direct"]` | Danh sách proxy tĩnh: `direct`, `http://…`, `socks5://…`, hoặc `#file:…`. |
| `proxyfile` | string | `""` | File chứa thêm proxy (mỗi dòng một proxy). Không lưu vào config. |
| `proxy_bindings` | object | — | Ghim một khoá upstream vào một proxy cụ thể. |

## 4. Upstream và retry

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `upstream.zen` | string | `https://opencode.ai/zen` | Origin tier Zen (shape OpenCode 1.x). |
| `upstream.go` | string | `https://opencode.ai/zen/go` | Origin tier Go (shape OpenCode 2.x). |
| `upstream.anonymous` | string | `https://opencode.ai/inference` | Origin free tier (`/inference`). |
| `retry.max_attempts` | int | `3` | Số lần thử tối đa: với lane anonymous là số exit tối thiểu được xoay. |
| `retry.timeout_seconds` | int | `300` | Hạn toàn bộ request (gồm retry). Stream dài nên để 900–1800. |

## 5. Mô hình

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `models.refresh_seconds` | int | `300` | Chu kỳ làm mới catalog. |
| `models.protocols` | object | `{}` | Ghi đè protocol upstream theo model id (`chat`, `responses`, `anthropic`, `systemone`). |
| `models.free_only` | bool | `false` | Chỉ phục vụ mô hình miễn phí (bản này bật `true` → 11 mô hình). |
| `models.active_probes` | bool | `false` | Probe chủ động để đo sẵn sàng (tốn quota); mặc định học thụ động từ request thật. |
| `models.anonymous_backoff` | object | — | Cầu dao lane miễn phí: `threshold` (6), `base_hours` (1), `max_hours` (24). |

## 6. Hiệu năng

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `performance.max_idle_conns` | int | `2048` | Tổng kết nối idle giữ lại. |
| `performance.max_idle_conns_per_host` | int | `256` | Idle mỗi host. |
| `performance.max_conns_per_host` | int | `0` | 0 = không giới hạn. |
| `performance.idle_conn_timeout_seconds` | int | `120` | Thời gian giữ kết nối idle. |
| `performance.connect_timeout_seconds` | int | `5` | Timeout bắt tay TCP/TLS. |
| `performance.failure_cooldown_seconds` | int | `15` | Thời gian tạm nghỉ của khoá/proxy sau lỗi. |
| `performance.attempt_timeout_seconds` | int | `0` | Hạn mỗi lần thử; 0 = dùng `retry.timeout_seconds`. |
| `performance.first_event_timeout_seconds` | int | `0` | Chờ byte/event đầu tiên; 0 = tắt. Nên đặt 120–240. |
| `performance.max_anonymous_exits` | int | `8` | Số exit tĩnh một request anonymous được đi qua. |
| `performance.free_tier_block_seconds` | int | `900` | Thời gian loại một exit bị free tier chặn IP. |
| `performance.model_timeouts` | object | `{}` | Hạn riêng theo model (giây), ghi đè `attempt_timeout_seconds`. |

## 7. Ghi log và WebUI

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `logging.level` | string | `info` | `debug`, `info`, `warn`, `error`. |
| `logging.ring_size` | int | `2000` | Số sự kiện log giữ trong bộ nhớ cho WebUI (100–50000). |
| `logging.dump_request_bodies` | bool | `false` | Ghi body gửi upstream (đã che bí mật, cắt ngắn) — chỉ bật khi debug. |
| `webui.enabled` | bool | `true` | Bật WebUI quản trị. **Cần restart** khi đổi. |
| `webui.listen` | string | `0.0.0.0:8081` | Địa chỉ WebUI. **Cần restart**. |
| `webui.username` | string | — | Tài khoản đăng nhập. |
| `webui.password` | string | — | Chỉ dùng để đặt mật khẩu; được hash vào `password_hash` rồi xoá. |
| `webui.session_ttl_minutes` | int | `720` | Thời hạn phiên đăng nhập. |

## 8. Suy luận và luân chuyển

| Trường | Kiểu | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `reasoning.effort` | string | `""` | Mức suy luận mặc định khi client không nêu (`none`/`auto`/`low`…`high`). |
| `reasoning.effort_by_model` | object | `{}` | Ghi đè theo model. |
| `rotation.sota.alias` | string | `sota` | Tên alias luân chuyển "mạnh nhất". |
| `rotation.sota.order` | string[] | — | Danh sách model theo thứ tự ưu tiên. |
| `rotation.sweet.alias` | string | `sweet` | Alias luân chuyển "nhẹ/nhanh". |
| `rotation.sweet.order` | string[] | `[]` | Thứ tự cho alias thứ hai. |
| `rotation.first_output_seconds` | int | `30` | Nếu model đầu chưa ra chữ trong N giây thì chuyển model kế. |

## 9. Cấu hình đang chạy (VPS2, đã che bí mật)

```json
{
  "listen": "0.0.0.0:6363",
  "anonymous": true,
  "warp": { "enabled": true, "pool_size": 16, "stack": "ipv6", "source_ipv6": "<prefix>/64" },
  "proxies": ["direct"],
  "retry": { "max_attempts": 4, "timeout_seconds": 1800 },
  "models": { "refresh_seconds": 300, "free_only": true },
  "performance": {
    "max_idle_conns": 2048,
    "max_idle_conns_per_host": 256,
    "max_conns_per_host": 0,
    "idle_conn_timeout_seconds": 90,
    "connect_timeout_seconds": 5,
    "failure_cooldown_seconds": 10,
    "attempt_timeout_seconds": 0,
    "first_event_timeout_seconds": 240,
    "max_anonymous_exits": 8,
    "free_tier_block_seconds": 900,
    "model_timeouts": {}
  },
  "logging": { "level": "info", "ring_size": 1000, "dump_request_bodies": false },
  "webui": { "enabled": true, "listen": "127.0.0.1:8092", "username": "admin", "session_ttl_minutes": 720 },
  "prefer": "go",
  "reasoning": { "effort": "high" }
}
```

## 10. Đo đạc tham khảo

| Chỉ số | Giá trị |
| --- | --- |
| RAM tiến trình (RSS) | 36–45 MB |
| Thời gian sẵn sàng sau restart | 26–40 s (pool 16, WARP lạnh) |
| Request 200 qua router | 6/6 chat, 3/3 stream trong lần kiểm tra cuối |
| Số mô hình mở | 11 (free) |
