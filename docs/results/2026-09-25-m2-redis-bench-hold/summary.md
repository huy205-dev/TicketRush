## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T10:42:50Z, commit `0cc0197`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 13595 | 13509 | 14541 |
| 201 (đơn tạo được) | 2430 | 2389 | 2433 |
| 409 SEATS_UNAVAILABLE | 11165 | 11120 | 12108 |
| RPS giữ ghế đỉnh (req/s) | 2901 | 2775 | 2971 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1350.9 | 1211.8 | 1510.6 |
| p50 (ms) | 47.67 | 44.03 | 63.07 |
| p95 (ms) | 502.16 | 454.16 | 542.12 |
| p99 (ms) | 958.08 | 764.51 | 1099.73 |
| max (ms) | 2197.59 | 2032.73 | 2286.15 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 14541 | 2433 / 12108 | 2775 | 63.07 | 542.12 | 1099.73 | 0 | 0 |
| 2 | 13509 | 2389 / 11120 | 2971 | 44.03 | 502.16 | 958.08 | 0 | 0 |
| 3 | 13595 | 2430 / 11165 | 2901 | 47.67 | 454.16 | 764.51 | 0 | 0 |
