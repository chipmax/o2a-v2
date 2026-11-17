# Relay egress cho free tier (Bun + Vercel)

Free tier của OpenCode có hai lớp khoá độc lập:

1. **TLS ClientHello phải đúng bộ cipher của Bun, đúng thứ tự** — gateway Go không
   tự giả được, nên request phải đi qua một tiến trình Bun.
2. **Hạn mức tính theo địa chỉ IP đi ra**, reset lúc **00:00 UTC** (upstream trả
   `Retry-After` đúng bằng số giây còn lại tới mốc đó).

Relay vì thế chỉ là một proxy mỏng chạy Bun: nó dial upstream bằng TLS của Bun,
báo địa chỉ egress của mình, và có thể tự kết thúc để request sau rơi vào một
instance khác.

```
o2a-v2  --HTTPS-->  relay (Bun/Vercel)  --Bun TLS-->  opencode.ai
        x-relay-*                    egress của instance
```

Địa chỉ egress **không phải IP tĩnh của project**: Vercel cấp địa chỉ theo
instance/phiên kết nối từ pool dùng chung của region, nên hai instance (và nhiều
lúc là hai kết nối) có thể ra bằng hai địa chỉ khác nhau. Đó là lý do một project
`fra1` phục vụ được hơn 250 request/ngày mà chưa chạm hạn mức: hạn mức bị chia
cho nhiều địa chỉ chứ không dồn vào một.

## 1. Deploy lên Vercel (khuyến nghị)

```bash
cd deploy/egress-selfhost/relay
vercel link
vercel env add POLYGATE_RELAY_TOKEN production   # chuỗi ngẫu nhiên >= 32 ký tự
vercel env add POLYGATE_ALLOWED_HOSTS production # *.opencode.ai,opencode.ai,api.kilo.ai
vercel deploy --prod
```

Danh sách host phải có `*.opencode.ai` vì gateway còn đọc metadata/catalog từ
`models.opencode.ai`; thiếu nó, `/v1/models` trả rỗng dù chat vẫn chạy.

`vercel.json` đã bật Bun runtime (`bunVersion: 1.x`), rewrite mọi path về
`api/relay.js` và cho phép chạy tới 300 s (đủ cho stream dài).

Mỗi project/region là **một pool IP riêng**. Muốn nhiều IP hơn thì tạo thêm
project ở region khác (`fra1`, `iad1`, `sin1`) — mỗi project là một entry
`relay://` trong cấu hình gateway.

## 2. Chạy thử tại máy

```bash
PORT=4318 POLYGATE_RELAY_TOKEN=devtoken POLYGATE_ALLOWED_HOSTS='*.opencode.ai,opencode.ai,api.kilo.ai' \
  bun api/relay.js            # Bun tự mở server cho export default { fetch } (xem PORT)
```

Kiểm tra:

```bash
curl -s -X POST http://127.0.0.1:4318/ \
  -H 'x-relay-key: devtoken' -H 'x-relay-control: ping'
# {"runtime":"bun 1.4.4","instance":"...","egress":"..."}
```

## 3. Khai báo trong gateway

Thêm vào `proxies` của `config.json` (token nằm ở userinfo, log đã được che):

```json
"proxies": [
  "relay://<POLYGATE_RELAY_TOKEN>@<project>.vercel.app",
  "relay://<token2>@<project-2>.vercel.app"
]
```

Luồng xử lý của gateway (`internal/relay`):

| Tình huống | Gateway làm gì |
| --- | --- |
| relay trả `x-relay-egress` | ghi nhớ IP đó cho request kế tiếp |
| `409` + `x-relay-rotate: 1` | IP đã nằm trong `x-relay-avoid` → recycle instance, thử lại |
| `429` + `FreeUsageLimit` | đánh dấu IP hết hạn mức **tới 00:00 UTC**, recycle, thử instance khác |
| `429` khác (rate limit tạm thời) | trả nguyên cho lane, không đánh dấu spent |
| `5xx` | recycle và thử lại (tối đa 4 lần) |
| hết lượt thử | trả `429 FreeUsageLimitError` để lane chuyển sang exit/tier khác |

`x-relay-avoid` được gửi kèm mọi lần thử, nên relay tự chối instance đã cháy
trước khi nó kịp gọi upstream.

## 4. Đo một relay

```bash
go run ./cmd/relayprobe -endpoint https://<project>.vercel.app -token <token> -pings 10 -burst 12
```

Ba dòng đáng đọc:

| Dòng | Ý nghĩa |
| --- | --- |
| `ping: N addresses over M requests` | càng nhiều địa chỉ thì project đó càng nhiều egress; `1` nghĩa là IP tĩnh (relay tại nhà, VPS) |
| `https://opencode.ai/... -> 200` | free lane còn hạn mức trên địa chỉ này |
| `burst: ... K address(es)` | số địa chỉ mà **cùng lúc** phục vụ được — đây là độ rộng thật của pool |

## 5. Giới hạn cần biết

- **Vercel Hobby** giới hạn thời gian chạy và băng thông; stream dài nên dùng
  gói trả phí hoặc chia nhỏ.
- Hạ tầng Vercel **thấy toàn bộ prompt**; không gửi dữ liệu nhạy cảm.
- Proxy hoá request qua Vercel có thể vi phạm AUP của họ — tự cân nhắc.
- Relay **không** dùng được cho Cloudflare Worker hay Deno Deploy: hai nền tảng
  này không đặt được bộ cipher của Bun nên upstream trả 429.
