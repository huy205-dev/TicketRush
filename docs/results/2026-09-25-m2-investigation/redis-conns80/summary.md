## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T11:56:44Z, commit `715259d`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=80, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14039 | 13712 | 14152 |
| 201 (đơn tạo được) | 2406 | 2374 | 2421 |
| 409 SEATS_UNAVAILABLE | 11618 | 11338 | 11746 |
| RPS giữ ghế đỉnh (req/s) | 2654 | 2446 | 2743 |
| RPS giữ ghế TB trên giây có tải (req/s) | 1054.8 | 884.5 | 1169.9 |
| p50 (ms) | 49.06 | 31.04 | 54.1 |
| p95 (ms) | 492.81 | 451.93 | 595.92 |
| p99 (ms) | 1057.2 | 908.35 | 1666.75 |
| max (ms) | 2651.37 | 2161.35 | 4513.3 |
| Tỉ lệ lỗi | 0 | 0 | 0 |
| p50 request thắng (201), k6 (ms) | 48.36 | 41.73 | 52.66 |
| p99 request thắng (201), k6 (ms) | 868.64 | 847.4 | 1227.94 |
| p50 request thua (409), k6 (ms) | 49.25 | 27.85 | 54.57 |
| p99 request thua (409), k6 (ms) | 1063.05 | 917.8 | 1750.39 |

Phía server, từ log booking (POST /v1/orders; thời gian trong process, không tính mạng và hàng đợi của k6):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| 201: p50 (ms) | 16.64 | 15.01 | 17.65 |
| 201: p99 (ms) | 108.58 | 91.24 | 125.17 |
| 201: p99 chờ pgxpool (ms) | 6.13 | 4.53 | 10.49 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.01 | 0 | 0.01 |
| 409: p50 (ms) | 1.72 | 1.66 | 1.92 |
| 409: p99 (ms) | 28.8 | 13.22 | 204.19 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 0.03 | 0.02 | 1.08 |
| 409: p99 thời gian query PG (ms) | 5.49 | 4.14 | 6.13 |
| 409: p99 thời gian Redis (ms) | 22.45 | 9.14 | 25.21 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.01 | 0.01 | 0.02 |
| Số lần acquire phải chờ (pool rỗng) | 113 | 109 | 215 |
| Tổng thời gian chờ khi pool rỗng (ms) | 617.68 | 542.77 | 1532.54 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 10.21 | 10.14 | 10.24 |
| booking (số core dùng trung bình) | 0.27 | 0.27 | 0.27 |
| Cả máy: % CPU bận, trung bình | 78.7 | 78.3 | 80.4 |
| Cả máy: % CPU bận, p90 theo giây | 82.4 | 82.1 | 85.3 |

- VU hết token: 1 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 14039 | 2421 / 11618 | 2743 | 49.06 | 492.81 | 908.35 | 0 | 0 |
| 2 | 14152 | 2406 / 11746 | 2654 | 31.04 | 451.93 | 1666.75 | 0 | 0 |
| 3 | 13712 | 2374 / 11338 | 2446 | 54.1 | 595.92 | 1057.2 | 0 | 0 |
