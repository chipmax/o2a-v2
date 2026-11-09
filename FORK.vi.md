# o2a-v2 — bản cá nhân hoá

Bản này dựa trên `opencode2api` 1.4.7 (Go, MIT). Mục tiêu: **một binary duy nhất**, WebUI
tiếng Việt, chạy gọn và ổn định hơn, không cần Python/Node/sing-box.

## Điểm khác biệt so với bản gốc

| Thay đổi | Lợi ích |
| --- | --- |
| WebUI mặc định **tiếng Việt** (vi / en / zh-CN / it) | Mở là dùng, không phải đổi ngôn ngữ |
| **Hạn mức theo từng khoá API**: danh sách mô hình, số request song song, số request/ngày | Chia sẻ gateway cho nhiều ứng dụng mà không sợ một chỗ ăn hết quota; trả `429` kèm `Retry-After` |
| **Timeout theo mô hình**: `performance.model_timeouts` | Mô hình suy luận chậm có ngân sách riêng, không cắt ngang stream dài |
| **Giữ nóng WARP**: lưu đăng ký vào `config.json.warp-state.json` | Khởi động lại không mất pool, không đăng ký lại từ đầu |
| **Nhận diện "200 + quota error"** của free tier | Không còn trả về client một body lỗi đội lốt thành công; tự xoay exit rồi trả `429 free_tier_quota_exhausted` |
| **401/403 từ key upstream** được đổi thành `502 upstream_credential_rejected` | Client không tưởng khoá của mình sai |
| Ẩn các trang thừa, gom menu Vận hành / Tích hợp / Mô hình / Hệ thống | Dễ hiểu hơn |

## Cài đặt trên VPS

```bash
# 1. Build (Go 1.24+)
go build -trimpath -ldflags "-s -w -X main.version=v2.0.0-dev" -o opencode2api ./cmd/opencode2api

# 2. Thư mục chạy
sudo install -d -o o2a -g o2a /opt/o2a-v2
sudo install -m 755 -o o2a -g o2a opencode2api /opt/o2a-v2/opencode2api
sudo install -m 600 -o o2a -g o2a config.json /opt/o2a-v2/config.json

# 3. systemd
sudo cp scripts/o2a-v2.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now o2a-v2
```

Hoặc chạy `sudo scripts/install.sh` để làm cả ba bước (script tự build nếu máy có Go).

Kiểm tra:

```bash
curl -s http://127.0.0.1:6363/healthz | jq
# ready=true, warp.active >= 1, models.exposed = số mô hình đang mở
```

Cổng mặc định của bản này: **6363** cho API (`listen`), WebUI đặt riêng trong
`webui.listen` (mặc định mẫu `0.0.0.0:8081`). Nếu dùng Oracle Cloud, phải mở
ingress TCP 6363 trong Security List/NSG thì mới truy cập được từ Internet.

## Cấu hình quan trọng

```json
{
  "listen": "0.0.0.0:6363",
  "anonymous": true,
  "warp": { "enabled": true, "pool_size": 16, "stack": "ipv6", "source_ipv6": "2603:...::/64" },
  "retry": { "max_attempts": 4, "timeout_seconds": 1800 },
  "performance": {
    "first_event_timeout_seconds": 240,
    "attempt_timeout_seconds": 0,
    "free_tier_block_seconds": 900,
    "model_timeouts": { "deepseek-v4.1-flash-free": 600 }
  },
  "models": { "free_only": true }
}
```

- `warp.pool_size`: càng lớn càng nhiều IP exit, quota free tier tính **theo IP exit**.
  16 là mức cân bằng; mỗi lần tăng cần thời gian làm nóng (đăng ký từng exit).
- `attempt_timeout_seconds: 0` nghĩa là dùng `retry.timeout_seconds`.
  `model_timeouts` ghi đè theo từng mô hình (giây).
- Free tier trả `429` hoặc **HTTP 200 kèm body lỗi quota**; gateway coi cả hai là lỗi
  và xoay exit, cuối cùng trả `429 free_tier_quota_exhausted`.

## Hạn mức theo khoá API

Trong WebUI: **Khoá API → nút "Hạn mức"** (hoặc sửa trực tiếp `config.json`).

```json
"server_key_metadata": {
  "<fingerprint 16 hex của khoá>": {
    "name": "dsh-pc",
    "allow_models": ["deepseek-v4.1-flash-free", "iceman-free"],
    "max_concurrency": 2,
    "daily_requests": 500
  }
}
```

- `allow_models` rỗng = mọi mô hình; áp dụng cả cho alias luân chuyển (`sota`, `sweet`).
- `0` = không giới hạn. Vượt hạn mức: `403 model_not_allowed`,
  `429 key_concurrency_limit`, `429 key_daily_limit` (đều có `Retry-After`).
- Hạn mức có hiệu lực ngay sau khi lưu, không cần khởi động lại.

## Mã lỗi thường gặp

| HTTP | `type` | Ý nghĩa / cách xử lý |
| --- | --- | --- |
| 401 | `authentication_error` | Sai khoá client gửi lên gateway |
| 403 | `model_not_allowed` | Khoá này không được phép dùng mô hình đó |
| 429 | `key_daily_limit` / `key_concurrency_limit` | Chạm hạn mức của khoá |
| 429 | `free_tier_quota_exhausted` | Hết quota free tier trên mọi exit; thêm key upstream, tăng `pool_size`, hoặc thử lại sau |
| 502 | `upstream_credential_rejected` | Key upstream của gateway bị từ chối (hết hạn / sai vùng) |
| 503 | `free_tier_ip_blocked` | OpenCode chặn IP exit; chờ hết `free_tier_block_seconds` |

## Chuyển toàn bộ gate về một cổng (peers)

Từ bản v2.4.0, `:6363` không cần router Python phía trước. Khai báo các gateway
khác trong `peers`; khi mọi lane nội bộ thất bại (429/hết quota, 5xx, 401/403),
request được chuyển tiếp nguyên văn sang peer, kèm header `X-Peer` cho biết ai đã phục vụ.

```json
"peers": [
  {
    "name": "vps1-o2a",
    "base_url": "http://10.0.0.184:8080",
    "key": "<server key của VPS1>",
    "tier": 1,
    "timeout_seconds": 180,
    "cooldown_seconds": 60,
    "models": []
  }
]
```

- `tier` nhỏ hơn được thử trước; cùng tier thì xoay vòng, nên hai máy ngang nhau chia tải.
- `429` tôn trọng `Retry-After` (tối đa 15 phút); lỗi khác nghỉ `cooldown_seconds`.
- `models: []` = phục vụ mọi mô hình; điền danh sách để giới hạn.
- Peer cũng được thử khi model không có trong catalog nội bộ, nên một địa chỉ vẫn
  trả lời được mô hình chỉ máy kia biết.
- Khi free tier báo hết quota cho một họ model, gateway ghi nhớ tới mốc reset UTC và
  **ưu tiên peer ngay từ đầu** cho các request sau — tránh mất 8–30 giây xoay exit vô ích.

Đo trên VPS2 (local đã hết quota họ `de…`): request đầu 9.3 s (học trạng thái),
các request sau 1.0–2.4 s, tất cả `200` với `X-Peer: vps1-o2a`.

## Trang “Ngân sách /64” (WebUI)

`GET /api/egress-quota` và mục **Ngân sách /64** trong WebUI liệt kê từng cặp
(`egress /64`, họ model) với: số request, số lần hết quota, trạng thái
`sẵn sàng / đang nghỉ / chưa dùng`, thời gian nghỉ còn lại và mốc reset UTC.
Dữ liệu lấy từ bộ đếm trong bộ nhớ cộng trạng thái cooling thật của pool WARP.

## Ghi chú vận hành

- `config.json.warp-state.json` chứa đăng ký WARP đã dùng; xoá file này để đăng ký mới hoàn toàn.
- Free tier tính quota theo IP exit: một VPS mới cần vài phút để pool tìm được exit còn quota.
- Log JSON có `event`: `request_routed`, `free_tier_quota_document`, `warp_429_exhausted`,
  `peer_served`, `peer_fallback`, `upstream_credential_rejected`, `key_daily_limit`... dùng để soi nhanh.
- Gateway không đổi mô hình khi client chỉ định rõ; chỉ alias luân chuyển mới đổi.
- Sau mỗi lần restart, pool WARP cần ~2–4 phút để ấm lại 8 exit; trong lúc đó peer
  (nếu có) gánh traffic.
