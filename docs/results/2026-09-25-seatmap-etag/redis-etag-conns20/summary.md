## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T15:05:29Z, commit `8aa4611`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=20, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

Số chính: latency phía server, đo trong booking cho POST /v1/orders (từ log booking, không tính mạng và hàng đợi của máy tạo tải):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14901 | 14761 | 15161 |
| 201 (đơn tạo được) | 2446 | 2443 | 2491 |
| 409 SEATS_UNAVAILABLE | 12455 | 12270 | 12718 |
| RPS giữ ghế đỉnh (req/s) | 4038 | 3733 | 4186 |
| RPS giữ ghế TB trên giây có tải (req/s) | 2952.2 | 2483.5 | 3032.2 |
| 201: p50 (ms) | 37.31 | 32.41 | 42.2 |
| 201: p99 (ms) | 237.6 | 129.62 | 254.43 |
| 409: p50 (ms) | 9.93 | 6.64 | 11.44 |
| 409: p99 (ms) | 207.53 | 203.8 | 219.39 |
| 201: p99 chờ pgxpool (ms) | 146.2 | 91.66 | 205.66 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.58 | 0.52 | 0.65 |
| 409: p50 chờ pgxpool (ms) | 3.98 | 0.96 | 6.51 |
| 409: p99 chờ pgxpool (ms) | 73.64 | 61.18 | 93.84 |
| 409: p99 thời gian query PG (ms) | 4.24 | 3.55 | 4.4 |
| 409: p99 thời gian Redis (ms) | 201.33 | 24.99 | 202.89 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.6 | 0.59 | 0.63 |
| Số lần acquire phải chờ (pool rỗng) | 11652 | 10684 | 12550 |
| Tổng thời gian chờ khi pool rỗng (ms) | 211041.11 | 174753.62 | 247988.87 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

Số từ k6 (k6 cùng máy, bị giới hạn CPU; chỉ để tham khảo):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| p50 (ms) | 32.61 | 26.75 | 44.31 |
| p95 (ms) | 158.8 | 125.27 | 229.85 |
| p99 (ms) | 346.78 | 334.32 | 406.29 |
| max (ms) | 951.3 | 892.48 | 1023.6 |
| p99 request thắng (201) (ms) | 310.95 | 305.62 | 330.43 |
| p99 request thua (409) (ms) | 358.23 | 340.3 | 431.25 |
| Sơ đồ ghế tải đầy đủ (200) | 4503 | 4215 | 4794 |
| Sơ đồ ghế không đổi (304) | 128739 | 128406 | 129015 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 1.11 | 1.03 | 1.16 |
| booking (số core dùng trung bình) | 0.24 | 0.23 | 0.24 |
| Cả máy: % CPU bận, trung bình | 21.1 | 18.9 | 24.4 |
| Cả máy: % CPU bận, p90 theo giây | 28.1 | 27.2 | 44.5 |

- VU hết token: 35 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms của k6 đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | Server p99 201 | Server p99 409 | k6 p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 14901 | 2446 / 12455 | 3733 | 237.6 | 207.53 | 346.78 | 0 | 0 |
| 2 | 15161 | 2443 / 12718 | 4038 | 254.43 | 219.39 | 334.32 | 0 | 0 |
| 3 | 14761 | 2491 / 12270 | 4186 | 129.62 | 203.8 | 406.29 | 0 | 0 |
