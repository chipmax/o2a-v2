# Providers tối giản sau khi loại bỏ

## 1. Bỏ khỏi kiến trúc

| Bỏ | Lý do |
|---|---|
| `pi-freeflow` (OMP/Pi) | Trùng nguồn với o2a v2 (Zen + Kilo), tự duy catalog riêng, tạo thêm daemon `:28180` và file log/cache riêng. Giữ o2a v2 làm provider OpenAI-compatible duy nhất cho OMP/Pi. |
| `polygate` (Hermes) | Gateway trung gian trùng `zen-gate`/`o2a-v2`, 27 model nhưng không thêm upstream mới. |
| OpenCode Go (subscription/OAuth) | Chưa có key thật, không nằm trong phạm vi vận hành; đã loại từ bản 17/11. Giữ code tier để tương thích nhưng **không cấu hình, không hiển thị**. |

Ba mục trên **không được xoá vật lý** khỏi máy trong đợt này — chỉ bị loại khỏi danh sách provider được dùng và khỏi tài liệu triển khai.

## 2. Provider còn lại

| Lớp | Provider giữ lại | Vai trò |
|---|---|---|
| Client | **DSH NEXT** | Dùng `commandcode` (`https://api.commandcode.ai/provider/v1`, 54 model) làm paid fallback; mọi free model đi qua `o2a-v2` |
| Client | **Hermes** | `o2a-v2 (VPS2)` (`http://161.118.246.9:8090/v1`) và `OpenKi (VPS2)` (endpoint `:8787` giữ nguyên) làm gateway VPS2; `o2a-vps (VPS1)` cho gateway cũ |
| Client | **OMP / Pi** | Một provider `o2a-v2 (VPS2)`, base URL `http://161.118.246.9:8090/v1`, key do router cấp |
| Gateway | **o2a-router :8090** | Front door duy nhất: tier 1 = o2a-v2 `:8000`, tier 2 = Kilo `api.kilo.ai/api/openrouter` |
| Gateway | **o2a-v2 :8000** | Catalog 43 model / 12 exposed; Zen key + anonymous; WARP/Proton egress |
| Egress | WARP (ipv6 trên VPS / dual trên PC) + 10 Proton SOCKS | Không phải model provider; giữ nguyên, lọc egress theo `/64` |

## 3. Sơ đồ mục tiêu

```
DSH NEXT ─┐
Hermes   ─┼──► o2a-router :8090 ──► o2a-v2 :8000 ──► OpenCode Zen (free)
OMP/Pi   ─┘                    └──► Kilo :free (fallback tier 2)

DSH NEXT ──► commandcode (paid fallback, riêng)
```

Mọi client dùng **cùng một base URL, cùng một danh sách model** do catalog của o2a quản lý. Không còn catalog riêng ở từng agent.

## 4. Các bước làm gọn

1. **Thống nhất endpoint**: thay mọi base URL `127.0.0.1:8787`, `127.0.0.1:18091`, `129.221.227.118:8080/8787` trong Hermes/OMP bằng `http://161.118.246.9:8090/v1`.
2. **Một danh sách model**: bỏ khối `models:` khai báo thủ công trong Hermes/DSH; dùng `GET /v1/models` từ router. Giữ alias tối thiểu (`sota`, `sweet`, `free`) do o2a cung cấp.
3. **Hermes**: xoá provider `polygate`, `o2a-vps`, `o2a`; chỉ giữ `o2a-v2`; đặt `model.provider: o2a-v2` cho mọi preset MOA.
4. **OMP/Pi**: gỡ extension `pi-freeflow` (`pi remove pi-freeflow` / `omp plugin uninstall pi-freeflow`), cấu hình provider mới trỏ thẳng tới router. Cache `~/.pi/agent/pi-freeflow-*` có thể xoá sau khi gỡ.
5. **DSH NEXT**: giữ `commandcode` làm provider paid duy nhất; xoá các provider `zen-gate*`, `o2a*` trùng nếu không còn dùng để so sánh. profile `cad` hiện có 5 provider → mục tiêu 2.
6. **o2a v2**: giữ Zen + anonymous + Kilo; không bật Go/OAuth; `config.example.json` bỏ `go` khỏi phần mô tả triển khai (code vẫn chấp nhận để tương thích).

## 5. Trạng thái hiện tại → mục tiêu

| Hệ thống | Provider hiện tại | Sau gọn |
|---|---|---|
| Hermes | polygate, `OpenKi (VPS2)`, `o2a-vps (VPS1)`, o2a, `o2a-v2 (VPS2)` (**5**) | `o2a-v2 (VPS2)` + `OpenKi (VPS2)` → **2** |
| DSH NEXT | commandcode, `o2a-vps (VPS1)`, `zen-gate-vps (VPS1)`, o2a, `OpenKi (VPS2)` (**5**) | commandcode + `o2a-v2 (VPS2)` → **2** |
| OMP/Pi | pi-freeflow (4 upstream riêng) | `o2a-v2 (VPS2)` → **1** |
| o2a router | o2a-v2 + Kilo | giữ nguyên **2 tier** |

Tổng số providerclient: **12 → 5**, trong đó chỉ còn **1 điểm cấu hình model** (catalog của o2a).

## 6. Rủi ro & rollback

- Hermes/DSH còn preset trỏ tới `zen-gate`/`polygate`: đổi `model.provider` trước khi xoá provider, nếu không agent sẽ lỗi `unknown provider`.
- OMP mất model picker tức thời nếu gỡ plugin trước khi cấu hình provider mới — làm bước 1–2 trước, bước 4 sau.
- Giữ backup config Hermes (`config.yaml.bak-providers-20261113T114538`) và profile DSH (`cordis.patch.yml.bak-commandcode-20261117-083358`) để khôi phục.
- Không đụng `go_*` trên VPS2: tier Go đang không có key, chỉ là code path dự phòng.

## 7. Đổi nhãn VPS đã áp dụng (17/11)

Trên **config thật của PC** (không đụng service trên VPS):

| Trước | Sau | Áp dụng ở |
|---|---|---|
| `o2a-v2` | `o2a-v2 (VPS2)` | Hermes + model/reference trong MOA |
| `zen-gate` (endpoint `:8787` giữ nguyên) | `OpenKi (VPS2)` | Hermes + DSH profile `cad` |
| `o2a-vps` | `o2a-vps (VPS1)` | Hermes + DSH profile `cad` |
| `zen-gate-vps` | `zen-gate-vps (VPS1)` | DSH profile `cad` |

- Mọi model id/name thuộc provider VPS đều được thêm hậu tố ` (VPS2)` hoặc ` (VPS1)`; reference `provider:`/`model:` trong MOA được cập nhật theo.
- Kiểm chứng: `yaml.safe_load` thành công, **0 reference trỏ tới provider không tồn tại**, **0 model thiếu hậu tố** ở các provider đã đổi.
- Số model đổi: Hermes 57 model + 25 reference; DSH 100 model (8 + 16 + 26 + các name tương ứng).
- Backup: `config.yaml.bak-rename-20261117135300` (Hermes), `cordis.patch.yml.bak-rename-20261117135300` (DSH).
- `polygate` và `pi-freeflow` vẫn còn trên máy — chỉ bị loại khỏi thiết kế; chưa gỡ vật lý.
