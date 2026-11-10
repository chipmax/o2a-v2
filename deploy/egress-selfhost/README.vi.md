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
