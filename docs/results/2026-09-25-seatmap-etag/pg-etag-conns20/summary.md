## bench-hold: backend pg, 3 lần

- Bắt đầu: 2026-09-25T15:10:11Z, commit `8aa4611`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=pg, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

Số chính: latency phía server, đo trong booking cho POST /v1/orders (từ log booking, không tính mạng và hàng đợi của máy tạo tải):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 15768 | 14137 | 15806 |
| 201 (đơn tạo được) | 2430 | 2423 | 2445 |
| 409 SEATS_UNAVAILABLE | 13345 | 11707 | 13361 |
| RPS giữ ghế đỉnh (req/s) | 2048 | 2044 | 2244 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1433.5 | 1413.7 | 1436.9 |
| 201: p50 (ms) | 343.91 | 304.08 | 447.05 |
| 201: p99 (ms) | 1066.18 | 843.76 | 1198.39 |
| 409: p50 (ms) | 513.76 | 387.68 | 572.32 |
| 409: p99 (ms) | 1019.77 | 941.87 | 1146.34 |
| 201: p99 chờ pgxpool (ms) | 1047.04 | 834.29 | 1187.72 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.95 | 0.95 | 0.95 |
| 409: p50 chờ pgxpool (ms) | 496.51 | 375.68 | 556.72 |
| 409: p99 chờ pgxpool (ms) | 1012.1 | 909.86 | 1131.6 |
| 409: p99 thời gian query PG (ms) | 85.86 | 84.47 | 86.69 |
| 409: p99 thời gian Redis (ms) | 4.45 | 3.93 | 4.87 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.97 | 0.97 | 0.97 |
| Số lần acquire phải chờ (pool rỗng) | 33640 | 30373 | 33804 |
| Tổng thời gian chờ khi pool rỗng (ms) | 7092413.83 | 4969821.37 | 7477303.33 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

Số từ k6 (k6 cùng máy, bị giới hạn CPU; chỉ để tham khảo):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| p50 (ms) | 440.64 | 360.76 | 542.94 |
| p95 (ms) | 904.99 | 877.42 | 923.16 |
| p99 (ms) | 1019.04 | 966.84 | 1146.91 |
| max (ms) | 1289.89 | 1040.77 | 1519.56 |
| p99 request thắng (201) (ms) | 1068.09 | 845.36 | 1199.76 |
| p99 request thua (409) (ms) | 1019.96 | 944.76 | 1149.76 |
| Sơ đồ ghế tải đầy đủ (200) | 6363 | 6128 | 6899 |
| Sơ đồ ghế không đổi (304) | 121146 | 119895 | 123370 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 1.28 | 1.24 | 1.35 |
| booking (số core dùng trung bình) | 0.28 | 0.27 | 0.29 |
| Cả máy: % CPU bận, trung bình | 24.0 | 20.9 | 26.0 |
| Cả máy: % CPU bận, p90 theo giây | 71.3 | 63.4 | 80.8 |

- VU hết token: 14 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms của k6 đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | Server p99 201 | Server p99 409 | k6 p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 15768 | 2423 / 13345 | 2044 | 1198.39 | 941.87 | 966.84 | 0 | 0 |
| 2 | 14137 | 2430 / 11707 | 2244 | 843.76 | 1019.77 | 1019.04 | 0 | 0 |
| 3 | 15806 | 2445 / 13361 | 2048 | 1066.18 | 1146.34 | 1146.91 | 0 | 0 |
