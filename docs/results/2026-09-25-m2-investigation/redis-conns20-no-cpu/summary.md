## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T11:35:17Z, commit `f253412`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14052 | 13072 | 14415 |
| 201 (đơn tạo được) | 2416 | 2347 | 2433 |
| 409 SEATS_UNAVAILABLE | 11705 | 10656 | 11982 |
| RPS giữ ghế đỉnh (req/s) | 3016 | 2889 | 3361 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1188.4 | 1171.0 | 1310.5 |
| p50 (ms) | 40.05 | 37.3 | 53.91 |
| p95 (ms) | 309.68 | 238.48 | 468.69 |
| p99 (ms) | 1037.16 | 704.47 | 1107.85 |
| max (ms) | 2074.21 | 1673.29 | 2220.54 |
| Tỉ lệ lỗi | 0 | 0 | 0 |
| p50 request thắng (201), k6 (ms) | 43.97 | 43.15 | 65 |
| p99 request thắng (201), k6 (ms) | 913.67 | 732.25 | 1184.44 |
| p50 request thua (409), k6 (ms) | 35.55 | 34.83 | 56.97 |
| p99 request thua (409), k6 (ms) | 1008.77 | 680.34 | 1142.67 |

Phía server, từ log booking (POST /v1/orders; thời gian trong process, không tính mạng và hàng đợi của k6):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| 201: p50 (ms) | 16.3 | 12.89 | 17.69 |
| 201: p99 (ms) | 73.46 | 63.16 | 74.82 |
| 201: p99 chờ pgxpool (ms) | 28.12 | 24.42 | 43.56 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.16 | 0.12 | 0.16 |
| 409: p50 (ms) | 1.74 | 1.45 | 1.89 |
| 409: p99 (ms) | 36.87 | 33.01 | 51.59 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 22.18 | 18.02 | 23.08 |
| 409: p99 thời gian query PG (ms) | 3.29 | 2.85 | 4.52 |
| 409: p99 thời gian Redis (ms) | 21.67 | 13.57 | 22.31 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.22 | 0.2 | 0.27 |
| Số lần acquire phải chờ (pool rỗng) | 2703 | 2485 | 3160 |
| Tổng thời gian chờ khi pool rỗng (ms) | 21250.28 | 18506.56 | 21538.42 |

- VU hết token: 0 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 13072 | 2416 / 10656 | 3016 | 40.05 | 309.68 | 1107.85 | 0 | 0 |
| 2 | 14415 | 2433 / 11982 | 3361 | 37.3 | 238.48 | 704.47 | 0 | 0 |
| 3 | 14052 | 2347 / 11705 | 2889 | 53.91 | 468.69 | 1037.16 | 0 | 0 |
