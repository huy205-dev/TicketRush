## bench-hold: backend pg, 3 lần

- Bắt đầu: 2026-09-25T10:38:48Z, commit `0cc0197`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=pg, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14652 | 14466 | 15311 |
| 201 (đơn tạo được) | 2421 | 2410 | 2435 |
| 409 SEATS_UNAVAILABLE | 12242 | 12031 | 12890 |
| RPS giữ ghế đỉnh (req/s) | 2070 | 1762 | 2154 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1177.8 | 1033.3 | 1221.0 |
| p50 (ms) | 461.72 | 446.16 | 489.33 |
| p95 (ms) | 973.58 | 728.45 | 1035.02 |
| p99 (ms) | 1061.53 | 959.31 | 1115.45 |
| max (ms) | 2205.08 | 1801.13 | 3047.79 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 15311 | 2421 / 12890 | 2154 | 461.72 | 973.58 | 1061.53 | 0 | 0 |
| 2 | 14652 | 2410 / 12242 | 2070 | 446.16 | 728.45 | 959.31 | 0 | 0 |
| 3 | 14466 | 2435 / 12031 | 1762 | 489.33 | 1035.02 | 1115.45 | 0 | 0 |
