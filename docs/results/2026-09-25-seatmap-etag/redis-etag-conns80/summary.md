## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T15:22:32Z, commit `8aa4611-dirty`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=80, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

Số chính: latency phía server, đo trong booking cho POST /v1/orders (từ log booking, không tính mạng và hàng đợi của máy tạo tải):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 15331 | 15184 | 16180 |
| 201 (đơn tạo được) | 2494 | 2477 | 2500 |
| 409 SEATS_UNAVAILABLE | 12854 | 12690 | 13680 |
| RPS giữ ghế đỉnh (req/s) | 4062 | 3988 | 4592 |
| RPS giữ ghế TB trên giây có tải (req/s) | 2555.2 | 2169.1 | 2696.7 |
| 201: p50 (ms) | 30.17 | 24.2 | 31.29 |
| 201: p99 (ms) | 261.96 | 248.92 | 316.61 |
| 409: p50 (ms) | 3.4 | 3.05 | 3.83 |
| 409: p99 (ms) | 210.09 | 208.45 | 218.89 |
| 201: p99 chờ pgxpool (ms) | 47.5 | 29.61 | 55.07 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.07 | 0.04 | 0.11 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 20.59 | 12.98 | 22.1 |
| 409: p99 thời gian query PG (ms) | 200.79 | 4.97 | 201.14 |
| 409: p99 thời gian Redis (ms) | 206.85 | 204.73 | 208.1 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.07 | 0.03 | 0.08 |
| Số lần acquire phải chờ (pool rỗng) | 2330 | 1566 | 2793 |
| Tổng thời gian chờ khi pool rỗng (ms) | 20114.07 | 11516.82 | 30210.99 |
| Tỉ lệ lỗi | 0 | 0 | 0 |

Số từ k6 (k6 cùng máy, bị giới hạn CPU; chỉ để tham khảo):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| p50 (ms) | 17.17 | 16.66 | 21.82 |
| p95 (ms) | 225 | 204.66 | 236.27 |
| p99 (ms) | 406.79 | 325.75 | 526.91 |
| max (ms) | 1657.01 | 1048.46 | 4551.49 |
| p99 request thắng (201) (ms) | 439.61 | 376.38 | 443.7 |
| p99 request thua (409) (ms) | 407.56 | 317.65 | 581.39 |
| Sơ đồ ghế tải đầy đủ (200) | 4258 | 4080 | 4799 |
| Sơ đồ ghế không đổi (304) | 129788 | 129010 | 129875 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 1.11 | 1.11 | 1.11 |
| booking (số core dùng trung bình) | 0.24 | 0.24 | 0.24 |
| Cả máy: % CPU bận, trung bình | 19.7 | 19.0 | 22.0 |
| Cả máy: % CPU bận, p90 theo giây | 25.6 | 22.7 | 25.9 |

- VU hết token: 18 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms của k6 đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | Server p99 201 | Server p99 409 | k6 p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 15184 | 2494 / 12690 | 4062 | 248.92 | 208.45 | 406.79 | 0 | 0 |
| 2 | 16180 | 2500 / 13680 | 4592 | 261.96 | 210.09 | 325.75 | 0 | 0 |
| 3 | 15331 | 2477 / 12854 | 3988 | 316.61 | 218.89 | 526.91 | 0 | 0 |
