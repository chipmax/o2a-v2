# Nguồn IP riêng cho free tier o2a (đo ngày 2026-11-10)

## 1. Kết luận đo được

| Đo | Kết quả |
| --- | --- |
| Hạn mức free tier | **theo từng IP egress, reset đúng 00:00 UTC (07:00 giờ VN)** — `Retry-After` luôn trỏ về 00:00 UTC |
| Dung lượng mỗi IP | khoảng 200 request/ngày (đo qua lần chạy thực tế; `quota-probe.py` sẽ chốt con số chính xác) |
| 15.231 proxy công khai | 353 kết nối được → 107 tới được `opencode.ai` → **107 × 429, 0 × 200** |
| 84 exit Tor (xoay circuit) | **84 × 429, 0 × 200** |
| WARP (34 exit) | toàn bộ 429 |
| IPv4 VPS2 (IP riêng, chưa dùng) | **200** trong 3,0 s cho tới khi hết hạn mức |

Kết luận: **pool công khai (proxy list, Tor, WARP) đã bị vắt cạn cho endpoint
này**. Chỉ IP **tự sở hữu và không chia sẻ** còn hạn mức, và mỗi IP chỉ được
~200 request/ngày. Vì vậy "nguồn IP ổn định" = **càng nhiều IPv4 công cộng
riêng càng tốt**, chứ không phải xoay proxy công khai.

## 2. Các hướng tự sở hữu IP (xếp theo công sức)

### A. Thêm IPv4 công cộng trên chính VPS2 (OCI) — 1 IP = 1 bucket

VNIC của VPS2 nhận được nhiều private IP, mỗi private IP gắn một **reserved
public IPv4**; mỗi địa chỉ là một bucket riêng. Đây là cách nhanh nhất, không
cần thêm máy.

Cần một lần cấu hình IAM trong OCI Console (quyền admin tenancy):

```text
Dynamic group: o2a-egress
  matching rule:
    instance.id = 'ocid1.instance.oc1.ap-singapore-1.anzwsljrcgl43iacmngj3swbt2ztlxtv347yeweyklqzsyq4g4qnheiwd6bq'

Policy (gắn ở tenancy root):
  Allow dynamic-group o2a-egress to manage vnics in tenancy
  Allow dynamic-group o2a-egress to manage private-ips in tenancy
  Allow dynamic-group o2a-egress to manage public-ips in tenancy
  Allow dynamic-group o2a-egress to read subnets in tenancy
  Allow dynamic-group o2a-egress to read instances in tenancy
```

Sau đó trên VPS2:

```bash
sudo /tmp/oci-venv/bin/python /opt/o2a-v2/egress-sync/oci-add-egress-ips.py --count 4 --apply --test
```

Script sẽ tạo private IP + reserved public IP, gắn vào `ens3`, rồi test
`api.ipify.org` và free tier cho từng địa chỉ. Lưu ý: OCI **tính phí IPv4 công
cộng reserved** (kiểm tra bảng giá hiện hành); IPv6 thì miễn phí.

### B. Thêm subnet IPv6 (bucket theo /64) — miễn phí

Nếu hạn mức IPv6 tính theo **/64** (giả thuyết mạnh: VPS1 và VPS2 cùng
`2603:c024:4526:9797::/64` và cùng 429 trong khi IPv4 VPS2 vẫn 200), thì mỗi
subnet mới = 1 bucket miễn phí. Cần tạo subnet + VNIC phụ (shape E2.1.Micro
thường cho 2 VNIC) rồi cấu hình IPv6 trên interface mới.

### C. Hurricane Electric 6in4 + routed /48 — nhiều /64 miễn phí

HE cấp **/48** định tuyến về VPS, tức 65.536 mạng /64. Nếu bucket IPv6 là /64
thì đây là nguồn gần như vô hạn, miễn phí.

1. Tạo tài khoản miễn phí tại <https://tunnelbroker.net>, tạo *Regular Tunnel*
   với IPv4 của VPS2, chọn PoP Singapore.
2. Trong trang tunnel, bấm *Create a Routed /48*.
3. Mở Security List của subnet trong OCI, cho phép **ingress protocol 41** từ
   IPv4 server của HE (và ICMPv6 nếu muốn ping).
4. Trên VPS2:

```bash
sudo ./he-ipv6-tunnel.sh setup <HE_SERVER_V4> <CLIENT_V6> <ROUTED_48>
sudo ./he-ipv6-tunnel.sh test <ROUTED_48> 6
```

`test` gán `::1` của 6 mạng /64 khác nhau và gửi free-tier request từ từng
địa chỉ: nếu ra 200 ở nhiều /64 thì giả thuyết /64 đúng và ta có pool lớn.

### D. Thêm VPS free tier (GCP e2-micro, AWS t2.micro, Azure B1s)

Mỗi máy một IPv4 công cộng riêng = một bucket ~200 request/ngày. Ổn định nhất
nhưng cần tài khoản và thời gian dựng.

## 3. Ước lượng nhu cầu

Mỗi IP ~200 request/ngày. Cần bao nhiêu IP tuỳ lưu lượng:

| Lưu lượng/ngày | Số IP cần |
| --- | --- |
| 200 | 1 |
| 600 | 3 |
| 1.000 | 5 |
| 2.000 | 10 |

## 4. Việc đang chạy tự động trên VPS2

- `o2a-egress-harvest.timer`: 00:06 UTC và mỗi 3 giờ, chạy
  `egress-harvest.py` — chỉ giữ exit trả **200** thật.
- `o2a-quota-probe.timer`: đã cài nhưng **tắt**, vì chạy lúc 00:03 UTC sẽ ăn
  hết hạn mức trong ngày của chính VPS2. Chỉ bật khi muốn đo dung lượng thật.
- `o2a-he-tunnel.service`: dựng tunnel 6in4 Hurricane Electric (`he-ipv6`) và
  dummy `he-v6` chứa routed /64; policy rule `from <routed>/64 lookup 100`.
- `o2a-he-reset-probe.timer`: 00:00:20 và 00:05:20 UTC, bắn 20 request từ 20
  địa chỉ /128 khác nhau trong routed /64 để đo hạn mức.

## 4b. KẾT LUẬN (2026-11-11, giờ VN) — chỉ WARP còn hạn mức, và theo "họ" /64

Bảng đo, mỗi dòng một nguồn egress khác nhau, cùng ngày:

| Egress | Cách tạo | Kết quả free lane |
| --- | --- | --- |
| VPS2 IPv4 `161.118.246.9` | direct | 429 |
| VPS2 IPv6 native `2603:c024:4526:9797::/64` | direct | 429 |
| HE routed `/64` `2001:470:36:965::/64`, 20 `/128` khác nhau | 6in4 | **0/25** |
| `sixrelay` (repo UWASDEV) 20 `/128` ngẫu nhiên trong HE /64 | SOCKS5 | **0/20** |
| nikeproxy residential VN `2407:5b40:0:419::/64` | SOCKS5/HTTP | 0/8 |
| IP residential nhà `171.241.6.97` (VNPT) | direct | 429 |
| 1.100 proxy công khai + 84 Tor exit | — | 0×200 |
| **Cloudflare WARP của VPS1** (`2a09:bac5:bf2x::/64`) | userspace WireGuard | **16/39 ≈ 41 %** |
| Cloudflare WARP của VPS2 (`2a09:bac5:55f8…55ff::/64`) | userspace WireGuard | **0/39** |

Đọc bảng:

1. **Không phải hạn mức theo IP.** 20 địa chỉ `/128` mới tinh trong một /64
   mới (HE) đều 429. Nếu là "mỗi IP 200 request" thì đã có 200.
2. **Không phải theo /64.** Cả /64 đó cũng mới; nếu bucket theo /64 thì cũng
   phải 200.
3. ⇒ Bucket thô hơn /64 (nhiều khả năng theo **/48** hoặc cả dải nhà cung
   cấp), nên mọi egress "dùng chung" (HE, OCI, VNPT/CGNAT, proxy, Tor) đều
   cạn. WARP là ngoại lệ vì mỗi lần đăng ký được cấp một `/64` trong một
   **họ /48 khác nhau**.
4. **WARP không phải vô hạn:** VPS2 nhận đúng một họ (`2a09:bac5:55f8–55ff`)
   và họ đó đã cạn (0/39); VPS1 nhận họ `2a09:bac5:bf2x` và họ đó còn sống
   (41 %). Đổi `warp.source_ipv6` giữa HE /64 và OCI /64 **không** đổi họ ⇒
   họ do phía Cloudflare gán theo thiết bị/đăng ký, không theo IP nguồn.
5. `Retry-After` của mọi 429 = số giây tới **00:00 UTC kế tiếp** ⇒ cửa sổ
   hạn mức là một ngày UTC, không phải rolling 24 h.

### Repo UWASDEV/Unlimited-Rotating-IPv6-Proxy-Server

Đã cài thật trên VPS2 (`install.sh --mode routed --subnet 2001:470:36:965::/64`),
self-test 6/6 PASS, 20 kết nối đồng thời cho **20 IPv6 nguồn khác nhau** — đúng
như quảng cáo. Nhưng 20 request free-tier qua nó: **0×200**. Kết luận: repo tốt,
giải đúng bài "xoay IP trong /64", nhưng bài toán ở đây không phải xoay IP.
Đã `install.sh --uninstall` để trả máy về trạng thái cũ.

### Vì sao VPS2 bật WARP mà vẫn 429

VPS2 nhận họ `2a09:bac5:55f8…55ff` (đã cạn). VPS2 cũng không có thư mục `data/`
nên mỗi lần restart lại đăng ký WARP mới, nhưng vẫn rơi vào cùng họ. Vì vậy
WARP trên VPS2 đã **tắt** (`warp.enabled=false`) để không làm chậm request;
VPS1 tiếp tục là node WARP (peer `vps1-o2a`).

### Việc tiếp theo (theo thứ tự)

1. **Xin HE /48** (bị chặn 24 h, thử lại sau 00:00 UTC): nếu /48 riêng cho hạn
   mức sạch thì HE thành nguồn ổn định (65 536 /64).
2. **Đăng ký WARP theo đường khác** để lấy họ /48 khác (xem
   `internal/warp/registrar.go`, thử endpoint registration khác hoặc đổi
   `warp.stack`).
3. Nếu cần ngay: dùng OpenCode Go (trả phí) — lane đó không bị 429.

## 5. Kiểm chứng 2026-11-11 (quan trọng — sửa lại giả thuyết /64)

Sau khi mở ICMP + protocol 41 ở Default Security List (làm bằng OCI Cloud
Shell, không cần dùng giao diện), tunnel HE đã chạy được:

```
he-ipv6  local 10.0.0.128 (private, OCI NAT ra 161.118.246.9)
client   2001:470:35:966::2/64
routed   2001:470:36:965::/64      (HE chỉ cấp /64, xin /48 trả "Error:")
egress   2001:470:36:965::1  -> api64.ipify.org OK
```

Kết quả đo hạn mức cùng lúc:

| Nguồn | /64 đã dùng chưa | Kết quả |
| --- | --- | --- |
| HE `2001:470:36:965::/64` (::1, ::2, ::abcd, ::beef::1, ::c1, ::c2) | chưa bao giờ | **429 toàn bộ** |
| OCI native `2603:c024:4526:9797::/64` (::b1…::b4 mới thêm) | một phần | 429 |
| VPS2 IPv4 `161.118.246.9` | đã dùng nhiều | 429 |
| IP residential nhà (Windows, VN) | mới trong ngày | **429** |
| `GET /inference/v1/models` | — | **200** |

Kết luận đúng hơn: **không phải bucket theo /64**. Một /64 hoàn toàn mới
(HE) vẫn 429, và IP residential cũng 429, trong khi `/models` trả 200 ⇒ hạn
mức free hiện là **một pool toàn cục cạn dần trong ngày** (hoặc chặn cả dải
tunnel/ASN), không phải "mỗi IP 200 request" như giả thuyết ban đầu.

Hệ quả: mua thêm proxy/IP lúc này không giải quyết gì. Việc cần làm là đo lại
ngay sau 00:00 UTC: nếu nhiều /128 trong cùng /64 cùng trả 200 thì bucket
theo IP là đúng và pool HE dùng được; nếu chỉ 1–2 địa chỉ 200 thì đó là pool
toàn cục và chỉ cần chờ/giãn nhịp.

## 6. CHỐT HƯỚNG IP (2026-11-11, 10:50 giờ VN) — dừng đầu tư IP

### 6.1. Key zen không mua thêm hạn mức theo IP

Ma trận `egress × lane`, cùng một thời điểm, model `deepseek-v4.1-flash-free`
và `big-pickle`:

| Egress | Ẩn danh | Key zen (`oc_sk_dad267ef…`) |
| --- | --- | --- |
| VPS2 direct | 429 | 429 |
| HE `2001:470:36:965::1` | 429 | 429 |

⇒ Key zen và lane ẩn danh **dùng chung một pool đã cạn**; đổi IP không đổi
kết quả. Script: `egress-lane-matrix.py`.

### 6.2. Bảy repo được rà (2026-11-11)

| Repo | Có gì | Đáng dùng? |
| --- | --- | --- |
| 12errh/zen-proxy | capture "wire shape" v2: `agent-preamble.txt`, `agent-tools.json`, `b3`/`traceparent`, `x-opencode-project` = sha1 | Tài liệu tốt; đã thử 6 biến thể (kể cả Bun 1.4.4) từ VPS2 → **429 cả 6** |
| B3hnamR/Polygate | gateway nhiều provider: OpenCode Free + **Kilo Free** + Z.ai | ⭐ Nguồn thật sự mới (xem 6.3) |
| rajwardhanspimple/OpenCodeWarp | relay Windows + xoay WARP bằng `registration delete` + `new`; probe bằng request thật | ⭐ Hai ý đã đưa vào code (6.4) |
| lipey1/opencode-warp-retry | plugin OpenCode xoay WARP khi `free-limit` | Xác nhận: xoay vài lần không được ⇒ hạn mức theo account |
| zhaohongsheng/warp-opencode | 1 file Python quản warp-cli/warp-plus/usque + bridge SOCKS5→HTTP | Thêm backend `usque` (MASQUE), không thêm quota |
| alztrk/opencode-ip-rotator | proxy FastAPI + WARP + pool + dashboard | Ngang o2a |
| leangtg123-art/opencode-termux-unlimited | Termux port của chính opencode2api, pool WARP 6 | Bản port của ta |

### 6.3. Nguồn miễn phí chạy được: Kilo Gateway

`https://api.kilo.ai/api/gateway`, không cần key, không cần WARP. Đo từ VPS2
(đúng IP đang bị OpenCode 429):

```
GET  /models   -> 200, 385 model, 11 model isFree
40 request vòng qua các model free -> 36×200, 4×403 (region), 0×429
```

Model dùng được: `kilo-auto/free`, `nvidia/nemotron-3.5-super-vl`,
`stealth/glyph-cluster`, `liquid/lfm-3-1.7b`, `dots-studio/dots-3-note-preview`,
`nvidia/nemotron-3.5-lightning`, `cohere/north-mini-code`,
`nvidia/nemotron-3-ultra-550b-a55b`, `nvidia/nemotron-3-nano-omni-30b-a3b-reasoning`,
`openrouter/free` (tất cả `tools: true`). Chỉ `thinkingmachines/inkling-2-small`
bị chặn theo vùng. Kilo cũng giới hạn theo **network address**
(429 `PROMOTION_MODEL_LIMIT_REACHED`) nhưng 40 request liên tiếp chưa chạm.
Script: `kilo-quota-probe.py`.

### 6.4. Sửa trong code (internal/warp)

1. **Admission probe**: mỗi exit mới phải trả lời một request thật
   (`LaneProbe`: `POST <upstream>/v1/chat/completions`, `max_tokens=1`, đủ
   header v2) trước khi pool nhận. 429 ⇒ log `admitted unverified` và exit bị
   đánh dấu spent (giữ lane sống, request thật đầu tiên sẽ xoay như cũ).
2. **Registration delete**: exit bị xoay/đánh dấu spent thì `DELETE /reg/{id}`
   trên Cloudflare khi đóng, để lần đăng ký sau là **thiết bị mới** (họ egress
   mới) thay vì tái dùng slot cũ.
3. Giữ nguyên: 401/403/429 ⇒ xoay exit; saved registration không còn được
   nhận mù sau restart nếu free lane từ chối nó.

Đo trên VPS2 với pool 2: `WARP exit … admitted unverified: free lane refused
this WARP exit: HTTP 429` → `egress 2a09:bac5:55fa:… ready` — đúng hành vi.

### 6.5. Quyết định

- **Đóng hướng mua/xoay IP.** Chín nguồn egress khác nhau, thêm lane key zen,
  thêm cả "wire shape" của CLI đều không vượt được hạn mức ⇒ nút thắt không
  nằm ở IP.
- VPS2: `warp.enabled=false`, `o2a-he-reset-probe.timer` đã `disable --now`
  (tunnel `he-v6` vẫn còn, có thể xoá khi cần).
- WARP chỉ còn là best-effort trên node nào tình cờ được cấp họ còn hạn mức;
  không dùng làm nguồn chính.
- Hướng còn lại có giá trị thật: **thêm lane Kilo** làm dự phòng khi 429
  (chưa tích hợp — cần map model và thêm provider vào gateway).

### 6.6. WARP qua MASQUE/QUIC (usque v4.2.1) — không cải thiện gì

Dựng `usque` trên VPS2 (đăng ký thiết bị mới qua `api.cloudflareclient.com`,
MASQUE = HTTP/3 over QUIC thay cho WireGuard), đo cùng lúc:

| Thiết bị | Egress quan sát (`cdn-cgi/trace`) | Kết quả |
| --- | --- | --- |
| 1 (QUIC, dual-stack) | `2a09:bac1:6560:8::3e6:70`, colo SIN, `warp=on` | **0/12** (429 toàn bộ) |
| 2 (QUIC) | `2a09:bac5:55fc:18c8::278:96` | 0/4 |
| 3 (QUIC) | `2a09:bac5:55fa:1d05::2e4:29` | 0/4 |
| 1 (HTTP/2 over TCP+TLS) | `2a09:bac5:55fc:25b9::3c2:f` | 0/4 |
| 1 (IPv6-only egress) | — | tunnel không lên được (`dial connect-ip: timeout`) |

Đọc kết quả:

1. MASQUE **có** cho họ egress khác WireGuard: `2a09:bac1::/48` (WireGuard chỉ
   ra `2a09:bac5::/48`). Nghĩa là protocol khác ⇒ prefix khác.
2. Nhưng **/48 khác vẫn 429** ⇒ giả thuyết "bucket theo /48" coi như bị bác;
   bucket nằm ở mức nhà cung cấp (toàn bộ WARP) hoặc pool toàn cục.
3. HTTP/2 (TCP 443, không cần UDP) hoạt động về mặt kết nối nhưng cùng kết quả
   ⇒ không phải vấn đề transport.
4. Không nên thêm backend `usque` vào gateway: cùng nguồn hạn mức đã cạn.

Cài đặt/thử lại: tải `usque_4.2.1_linux_amd64.zip`, `usque register --accept-tos`,
rồi `usque socks -b 127.0.0.1 -p 1081` (lưu ý `-p` là cổng SOCKS, `-P` là cổng
kết nối MASQUE).

## 7. Năm repo WARP khác (2026-11-11, 12:00 giờ VN) — chỉ giúp độ trễ, không giúp hạn mức

| Repo | Nội dung thật | Dùng được gì |
| --- | --- | --- |
| DarknessShade/Warp-Endpoint-Scanner | quét endpoint WARP hằng ngày (GitHub Action), xuất bảng latency/loss + `export/warp-ip` | Danh sách endpoint tốt để chọn cổng nhanh; **không** đổi egress |
| kwlnd/warp-unblock | app Windows (Rust + wgcf + mihomo + zapret + opera-proxy) để vượt DPI của ISP Nga | Không áp dụng cho VPS Linux; vấn đề ở đây không phải DPI |
| LK46DPA/Warp_Switcher | GUI egui bật/tắt `warp-cli` | Tác giả đã tự ghi "không còn cần thiết"; không có gì mới |
| vio137/wax-vpn | client WARP viết Rust (GUI, WireGuard + MASQUE) | Chỉ là client khác; cùng nguồn hạn mức |
| Daey886/warp-nodes | 37 node = **một thiết bị WARP+** (license 1,84 PB) dùng chung, kèm list endpoint | Đo được cơ chế mới (7.1) và xác nhận WARP+ không giúp (7.2) |

### 7.1. Egress đổi khi **bắt tay lại**, không đổi khi đổi endpoint trên tunnel đang chạy

Đo trên VPS2, cùng một thiết bị free:

| Cách đổi | Endpoint | Egress |
| --- | --- | --- |
| `wg set … endpoint` (tunnel đang chạy), 9 endpoint | 162.159.192.199:2408 … 8.6.112.215:500 | **một địa chỉ duy nhất** `2a09:bac5:55fa:30c8::4dc:b2` |
| `wg-quick down/up` với endpoint khác, 6 lần | :878, :987, :2408, :4500, :5956 | **6 địa chỉ khác nhau**: `2a09:bac1:6500:8::278:a0`, `2a09:bac5:55fd:25c3::3c3:34`, `2a09:bac5:55fa:25cd::3c4:1e`, `2a09:bac5:55fa:18c8::278:80`, `2a09:bac1:6560:8::4dc:41`, `2a09:bac5:55f9:101e::19b:15a` |

⇒ Egress do **phiên bắt tay** quyết định (và cả theo cổng: cùng host
`162.159.192.19`, cổng `:987` ra `bac1`, cổng `:878` ra `bac5`). Vì vậy xoay
egress **không cần đăng ký thiết bị mới** — chỉ cần down/up với endpoint khác,
tránh giới hạn ~9 registration/giờ của Cloudflare. Đây là điểm đáng đưa vào
`internal/warp` (hiện mỗi lần xoay đều đăng ký thiết bị mới).

### 7.2. Cả 6 egress mới đều 429 — kể cả thiết bị WARP+

| Thiết bị | Egress | Free lane |
| --- | --- | --- |
| free, reconnect ×6 | `bac1:6500`, `bac5:55fd`, `bac5:55fa` ×2, `bac1:6560`, `bac5:55f9` | **0/18** |
| WARP+ dùng chung (warp-nodes) | `bac1:6520:8::2e4:7b`, `bac1:6520:8::3e6:50`, `bac5:55ff:137d::1f1:1e7`, `bac5:55fc:25cd::3c4:3d` | 0/8 |

⇒ Hạn mức **không** theo egress, **không** theo /48, **không** theo gói WARP+
— nó nằm ở phía OpenCode và đang cạn cho toàn bộ WARP. Kết luận mục 6.5 giữ
nguyên; 5 repo này không mở thêm đường nào.

### 7.3. Việc rút ra cho code (chưa làm)

`internal/warp` nên thử **reconnect-resample** trước khi đăng ký thiết bị mới:
giữ registration, `down/up` với endpoint kế tiếp, chạy `admit`, chỉ đăng ký mới
khi resample thất bại. Lợi: không chạm giới hạn registration, không tạo rác
thiết bị. Chưa làm vì mọi egress hiện đều 429 nên không có lợi ích đo được;
để dành khi hạn mức sống lại.



