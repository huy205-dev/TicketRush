## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T11:47:33Z, commit `715259d`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14295 | 13923 | 14609 |
| 201 (đơn tạo được) | 2395 | 2392 | 2438 |
| 409 SEATS_UNAVAILABLE | 11900 | 11485 | 12217 |
| RPS giữ ghế đỉnh (req/s) | 3414 | 3239 | 3485 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1429.5 | 1160.2 | 1460.9 |
| p50 (ms) | 32.84 | 25.05 | 39.51 |
| p95 (ms) | 483.24 | 351.33 | 532.28 |
| p99 (ms) | 1102.39 | 833.47 | 1465.99 |
| max (ms) | 2497.06 | 2002.88 | 3188.06 |
| Tỉ lệ lỗi | 0 | 0 | 0 |
| p50 request thắng (201), k6 (ms) | 35.35 | 32.24 | 44.22 |
| p99 request thắng (201), k6 (ms) | 849.02 | 646.25 | 952.89 |
| p50 request thua (409), k6 (ms) | 31.88 | 22.84 | 38.25 |
| p99 request thua (409), k6 (ms) | 1134.8 | 820.76 | 1549.1 |

Phía server, từ log booking (POST /v1/orders; thời gian trong process, không tính mạng và hàng đợi của k6):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| 201: p50 (ms) | 12.55 | 11.31 | 14.74 |
| 201: p99 (ms) | 54.75 | 51.36 | 64.81 |
| 201: p99 chờ pgxpool (ms) | 24.67 | 17.43 | 26.12 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.1 | 0.09 | 0.15 |
| 409: p50 (ms) | 1.38 | 1.32 | 1.69 |
| 409: p99 (ms) | 24.82 | 13.89 | 27.71 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 10.51 | 8.81 | 16.68 |
| 409: p99 thời gian query PG (ms) | 3.58 | 2.43 | 3.74 |
| 409: p99 thời gian Redis (ms) | 13.13 | 7.76 | 16.25 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.16 | 0.13 | 0.19 |
| Số lần acquire phải chờ (pool rỗng) | 2306 | 1632 | 3526 |
| Tổng thời gian chờ khi pool rỗng (ms) | 10731.09 | 7254.66 | 21111.13 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 10.7 | 9.79 | 10.73 |
| booking (số core dùng trung bình) | 0.25 | 0.25 | 0.26 |
| Cả máy: % CPU bận, trung bình | 79.4 | 79.4 | 79.6 |
| Cả máy: % CPU bận, p90 theo giây | 82.6 | 82.4 | 84.7 |

- VU hết token: 1 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 14295 | 2395 / 11900 | 3485 | 32.84 | 532.28 | 1102.39 | 0 | 0 |
| 2 | 13923 | 2438 / 11485 | 3414 | 25.05 | 351.33 | 1465.99 | 0 | 0 |
| 3 | 14609 | 2392 / 12217 | 3239 | 39.51 | 483.24 | 833.47 | 0 | 0 |
