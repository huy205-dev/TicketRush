## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T15:17:49Z, commit `8aa4611`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention_full.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

Số chính: latency phía server, đo trong booking cho POST /v1/orders (từ log booking, không tính mạng và hàng đợi của máy tạo tải):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 13296 | 12410 | 13997 |
| 201 (đơn tạo được) | 2389 | 2338 | 2397 |
| 409 SEATS_UNAVAILABLE | 10958 | 10021 | 11600 |
| RPS giữ ghế đỉnh (req/s) | 2819 | 2620 | 3176 |
| RPS giữ ghế TB trên giây có tải (req/s) | 886.4 | 886.4 | 1166.4 |
| 201: p50 (ms) | 17.87 | 15.25 | 20.6 |
| 201: p99 (ms) | 80.26 | 62.23 | 81.39 |
| 409: p50 (ms) | 1.93 | 1.87 | 1.98 |
| 409: p99 (ms) | 26.92 | 19.26 | 29.82 |
| 201: p99 chờ pgxpool (ms) | 31.15 | 28.63 | 32.88 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.15 | 0.15 | 0.16 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 19.01 | 13.71 | 22.04 |
| 409: p99 thời gian query PG (ms) | 3.66 | 3.06 | 3.86 |
| 409: p99 thời gian Redis (ms) | 10.5 | 9.68 | 11.02 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.26 | 0.22 | 0.27 |
| Số lần acquire phải chờ (pool rỗng) | 3056 | 3024 | 3254 |
| Tổng thời gian chờ khi pool rỗng (ms) | 20809.01 | 17297.09 | 24747.28 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

Số từ k6 (k6 cùng máy, bị giới hạn CPU; chỉ để tham khảo):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| p50 (ms) | 33.59 | 30.7 | 38.94 |
| p95 (ms) | 434.85 | 364.31 | 570.43 |
| p99 (ms) | 1100.28 | 940.25 | 1810.94 |
| max (ms) | 3407.15 | 2043.17 | 3691 |
| p99 request thắng (201) (ms) | 1061.88 | 738.57 | 1664.76 |
| p99 request thua (409) (ms) | 1102.84 | 940.68 | 1842.1 |
| Sơ đồ ghế tải đầy đủ (200) | null | null | null |
| Sơ đồ ghế không đổi (304) | 0 | 0 | 0 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 10.00 | 9.95 | 10.13 |
| booking (số core dùng trung bình) | 0.26 | 0.26 | 0.26 |
| Cả máy: % CPU bận, trung bình | 80.1 | 79.9 | 80.9 |
| Cả máy: % CPU bận, p90 theo giây | 84.7 | 83.3 | 85.3 |

- VU hết token: 0 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms của k6 đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | Server p99 201 | Server p99 409 | k6 p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 13296 | 2338 / 10958 | 2620 | 80.26 | 29.82 | 1100.28 | 0 | 0 |
| 2 | 12410 | 2389 / 10021 | 2819 | 81.39 | 26.92 | 1810.94 | 0 | 0 |
| 3 | 13997 | 2397 / 11600 | 3176 | 62.23 | 19.26 | 940.25 | 0 | 0 |
