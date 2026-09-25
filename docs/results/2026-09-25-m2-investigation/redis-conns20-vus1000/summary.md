## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T12:02:17Z, commit `715259d`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=1000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 11879 | 11579 | 11958 |
| 201 (đơn tạo được) | 2389 | 2380 | 2415 |
| 409 SEATS_UNAVAILABLE | 9464 | 9199 | 9569 |
| RPS giữ ghế đỉnh (req/s) | 3109 | 3038 | 3378 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1697.0 | 1654.1 | 1708.3 |
| p50 (ms) | 20.29 | 19.49 | 20.32 |
| p95 (ms) | 163.47 | 163.03 | 187.01 |
| p99 (ms) | 374.9 | 338.33 | 414.64 |
| max (ms) | 940.03 | 898.55 | 1045.6 |
| Tỉ lệ lỗi | 0 | 0 | 0 |
| p50 request thắng (201), k6 (ms) | 24.58 | 23.73 | 24.68 |
| p99 request thắng (201), k6 (ms) | 349.64 | 339.43 | 383.24 |
| p50 request thua (409), k6 (ms) | 19.05 | 17.99 | 19.29 |
| p99 request thua (409), k6 (ms) | 369.35 | 337.21 | 428.44 |

Phía server, từ log booking (POST /v1/orders; thời gian trong process, không tính mạng và hàng đợi của k6):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| 201: p50 (ms) | 9.85 | 9.3 | 10.63 |
| 201: p99 (ms) | 45.8 | 45.18 | 46.52 |
| 201: p99 chờ pgxpool (ms) | 13.23 | 11.36 | 25.53 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.06 | 0.05 | 0.1 |
| 409: p50 (ms) | 1.69 | 1.41 | 1.75 |
| 409: p99 (ms) | 14.84 | 12.88 | 16.25 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 8.69 | 7.85 | 11.27 |
| 409: p99 thời gian query PG (ms) | 2.64 | 2.28 | 3.59 |
| 409: p99 thời gian Redis (ms) | 8.94 | 7.13 | 11.51 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.12 | 0.12 | 0.13 |
| Số lần acquire phải chờ (pool rỗng) | 1479 | 1283 | 1609 |
| Tổng thời gian chờ khi pool rỗng (ms) | 5739.73 | 4145.29 | 6699.4 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 9.9 | 8.51 | 10.59 |
| booking (số core dùng trung bình) | 0.23 | 0.23 | 0.26 |
| Cả máy: % CPU bận, trung bình | 73.8 | 67.6 | 78.3 |
| Cả máy: % CPU bận, p90 theo giây | 78.3 | 74.0 | 85.5 |

- VU hết token: 28 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 11579 | 2380 / 9199 | 3038 | 20.32 | 163.03 | 414.64 | 0 | 0 |
| 2 | 11958 | 2389 / 9569 | 3378 | 20.29 | 187.01 | 338.33 | 0 | 0 |
| 3 | 11879 | 2415 / 9464 | 3109 | 19.49 | 163.47 | 374.9 | 0 | 0 |
