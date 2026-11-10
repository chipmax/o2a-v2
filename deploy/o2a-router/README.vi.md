# o2a-router (tuỳ chọn)

Router Python nhỏ đứng trước gateway, dùng khi cần:

- gộp nhiều gateway (VPS1/VPS2) và chuyển khi một bên lỗi hoặc hết quota;
- một khoá duy nhất cho agent (`listen_port`, mặc định 8090);
- làn Kilo keyless cho các model `:free` của Kilo Code, không đi qua WARP.

## Kilo lane

Thêm một upstream `"name": "kilo"`, `"base": "https://api.kilo.ai/api/openrouter"`,
`"strip_v1": true`, không cần `key`. Đặt `kilo_models` = các model ID Kilo mà
router được phép nhận, `kilo_upstreams` = `["kilo"]`, và `kilo_catalog` = metadata
để `/v1/models` trả kèm context/max output.

Router chỉ chuyển sang Kilo khi:

- request là `POST .../chat/completions` (OpenAI);
- `model` nằm đúng trong `kilo_models`.

Mọi model khác vẫn đi gateway như cũ; Kilo không bao giờ được dùng làm fallback
cho model của gateway.

## Chạy

```bash
sudo cp router.py config.json /opt/o2a-router/
sudo systemctl restart o2a-router
curl -H "Authorization: Bearer <router-key>" http://127.0.0.1:8090/v1/models
```

Sao lưu `router.py` và `config.json` trước khi thay; `config.example.json` chỉ là
mẫu, thay khoá và `ui_base` theo máy.
