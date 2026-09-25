## bench-hold: backend redis, 3 lần

- Bắt đầu: 2026-09-25T11:52:07Z, commit `715259d`
- Máy: Apple M5 Pro, 15 logical CPUs, 24 GiB RAM, macOS 26.4; Docker VM: 15 CPU, 11.7 GiB
- PostgreSQL 16.15, shared_buffers=128MB, max_connections=100; booking: INVENTORY_BACKEND=redis, DB_MAX_CONNS=40, HOLD_TTL=10m0s, HOLD_GRACE=30s
- k6 v2.3.0, kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, VUS=2000, RAMP=10s, HOLD=60s; reset DB + flush Redis + seed trước mỗi lần

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14584 | 13459 | 15622 |
| 201 (đơn tạo được) | 2452 | 2391 | 2456 |
| 409 SEATS_UNAVAILABLE | 12132 | 11068 | 13166 |
| RPS giữ ghế đỉnh (req/s) | 2568 | 2463 | 2784 |
| RPS giữ ghế TB trên giây có tải (req/s) | 911.5 | 897.3 | 1041.5 |
| p50 (ms) | 55.34 | 43.96 | 84.48 |
| p95 (ms) | 538.7 | 446.94 | 562.14 |
| p99 (ms) | 881.91 | 755.07 | 1197.7 |
| max (ms) | 3351.67 | 3101.23 | 4992.04 |
| Tỉ lệ lỗi | 0 | 0 | 0 |
| p50 request thắng (201), k6 (ms) | 61.78 | 43.66 | 71.46 |
| p99 request thắng (201), k6 (ms) | 734.57 | 720.06 | 749.39 |
| p50 request thua (409), k6 (ms) | 53.38 | 44.06 | 89.41 |
| p99 request thua (409), k6 (ms) | 905.17 | 755.65 | 1250.65 |

Phía server, từ log booking (POST /v1/orders; thời gian trong process, không tính mạng và hàng đợi của k6):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| 201: p50 (ms) | 17.67 | 15.39 | 21.9 |
| 201: p99 (ms) | 87.14 | 71.54 | 247.82 |
| 201: p99 chờ pgxpool (ms) | 10.55 | 9.78 | 21.31 |
| 201: tỉ lệ thời gian chờ pgxpool | 0.02 | 0.01 | 0.03 |
| 409: p50 (ms) | 1.82 | 1.7 | 2.48 |
| 409: p99 (ms) | 207.04 | 23.5 | 220.51 |
| 409: p50 chờ pgxpool (ms) | 0 | 0 | 0 |
| 409: p99 chờ pgxpool (ms) | 3.4 | 2.87 | 26.11 |
| 409: p99 thời gian query PG (ms) | 5.28 | 3.81 | 9.93 |
| 409: p99 thời gian Redis (ms) | 203.76 | 19.05 | 210.1 |
| 409: tỉ lệ thời gian chờ pgxpool | 0.02 | 0.01 | 0.09 |
| Số lần acquire phải chờ (pool rỗng) | 505 | 467 | 1928 |
| Tổng thời gian chờ khi pool rỗng (ms) | 1886.25 | 1840.97 | 14506.26 |

CPU trong lúc k6 chạy (máy có 15 CPU logic; thời gian CPU chia cho thời gian thực):

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| k6 (số core dùng trung bình) | 9.97 | 9.83 | 10.38 |
| booking (số core dùng trung bình) | 0.27 | 0.26 | 0.27 |
| Cả máy: % CPU bận, trung bình | 79.4 | 78.8 | 79.8 |
| Cả máy: % CPU bận, p90 theo giây | 86.3 | 82.4 | 86.5 |

- VU hết token: 0 lần (phải là 0, nếu không kịch bản đã bị cắt bớt)
- Ngưỡng p99 < 200 ms đạt ở 0/3 lần
- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở 3/3 lần; dòng ERROR trong log booking: 0

| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 15622 | 2456 / 13166 | 2784 | 55.34 | 538.7 | 1197.7 | 0 | 0 |
| 2 | 14584 | 2452 / 12132 | 2568 | 84.48 | 562.14 | 881.91 | 0 | 0 |
| 3 | 13459 | 2391 / 11068 | 2463 | 43.96 | 446.94 | 755.07 | 0 | 0 |
