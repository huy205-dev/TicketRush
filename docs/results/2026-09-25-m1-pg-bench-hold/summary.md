## bench-hold: backend pg, 3 lần

- Bắt đầu: 2026-09-25T10:06:28Z, commit `99d3e6d`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=pg, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14776 | 14275 | 14918 |
| 201 (đơn tạo được) | 2439 | 2431 | 2444 |
| 409 SEATS_UNAVAILABLE | 12337 | 11844 | 12474 |
| RPS giữ ghế đỉnh (req/s) | 2124 | 2086 | 2184 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1189.6 | 923.5 | 1243.2 |
| p50 (ms) | 399.12 | 383.16 | 503.35 |
| p95 (ms) | 803.77 | 766.12 | 1342.47 |
| p99 (ms) | 970.76 | 875.39 | 1503.52 |
| max (ms) | 2392.4 | 2016.61 | 4705.39 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 14776 | 2439 / 12337 | 2124 | 503.35 | 1342.47 | 1503.52 | 0 | 0 |
| 2 | 14918 | 2444 / 12474 | 2184 | 383.16 | 766.12 | 970.76 | 0 | 0 |
| 3 | 14275 | 2431 / 11844 | 2086 | 399.12 | 803.77 | 875.39 | 0 | 0 |
