# Kết quả đo

Chỉ ghi kết quả từ lần chạy thật. Mỗi lần đo ghi: ngày, commit hash, cấu hình máy, kịch bản, RPS, p50/p95/p99, tỉ lệ lỗi, kết quả invariants, ghi chú thay đổi so với lần trước.

Số liệu chạy k6 cùng máy với hệ thống chỉ dùng để so sánh tương đối (k6 tranh CPU với service). Số liệu chính thức chạy trên VPS riêng.

## Baseline PG

Backend `INVENTORY_BACKEND=pg`: giữ ghế bằng `SELECT … FOR UPDATE` trên bảng `seats` rồi ghi `seat_holds` (xem [ADR-000](adr/000-ban-goc-postgresql.md)).

### Cấu hình

| | |
|---|---|
| Máy | MacBook, Apple M5 Pro (15 core: 5 performance + 10 efficiency), 24 GiB RAM, macOS 26.4 |
| Container | OrbStack 2.2.3, VM 15 CPU / 12 GiB |
| PostgreSQL | 16.15 (image `postgres:16.15-alpine`, cấu hình mặc định: `shared_buffers=128MB`, `max_connections=100`, `synchronous_commit=on`) |
| Booking | Binary build từ commit, chạy thẳng trên macOS (không trong container). `DB_MAX_CONNS=20`, `HOLD_TTL=10m`, `HOLD_GRACE=30s`, log JSON ghi ra file |
| Công cụ | Go 1.27.1, k6 v2.3.0 chạy cùng máy |
| Kịch bản | [`loadtest/hold_contention.js`](../loadtest/hold_contention.js): ramp 0 → 2.000 VU trong 10 s, giữ 60 s, 70% VU nhắm khu VIP. Chạy `make db-reset seed` trước mỗi lần |

**Cách tính**
- *RPS hold đỉnh*: số request `POST /v1/orders` trong giây đông nhất, tính bằng [`loadtest/peak_rps.sh`](../loadtest/peak_rps.sh) từ CSV của k6.
- *RPS hold TB*: tổng request hold chia cho số giây có request hold.
- Latency: `http_req_duration{name:hold}` của k6.
- Tỉ lệ lỗi: `http_req_failed{name:hold}`; 409 được tính là phản hồi hợp lệ (ghế đã có người giữ là kết quả nghiệp vụ bình thường).
- Bất biến: `tools/invariants` chưa có (M7), nên sau mỗi lần chạy mình kiểm tra bằng SQL: không ghế nào thuộc hai đơn `HELD`; mọi `seat_holds` còn hạn khớp 1–1 với `order_seats` của đơn `HELD`; số dòng `outbox` bằng số đơn. Log booking không có dòng `ERROR`/`WARN` nào.

`BUYER_MODE` là hai cách hiểu câu "Mỗi VU: dev-login, xem ghế, …" của SPEC (chi tiết ở [ADR-006](adr/006-lua-chon-khi-spec-chua-ro-m1.md)):
- `vu`: mỗi VU là một người mua, đặt được một đơn thì dừng.
- `iteration`: mỗi vòng lặp là một người mua mới, VU đặt tiếp đến khi hết ghế.

### Kết quả

| Lần | Ngày | Commit | BUYER_MODE | Request hold | 201 / 409 | RPS hold đỉnh | RPS hold TB | p50 | p95 | p99 | max | Lỗi | Ngưỡng p99 < 200 ms | Bất biến |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 2026-09-25 | `e1c5b98` | vu | 3.728 | 2.000 / 1.728 | 888 | 339 | 3,13 ms | 35,40 ms | 175,19 ms | 245,45 ms | 0% | đạt | đạt |
| 2 | 2026-09-25 | `ec8af67` | vu | 3.901 | 2.000 / 1.901 | 765 | 355 | 3,04 ms | 17,01 ms | 35,52 ms | 56,32 ms | 0% | đạt | đạt |
| 3 | 2026-09-25 | `ec8af67` | iteration | 14.825 | 2.447 / 12.378 | 2.209 | 1.235 | 347,99 ms | 790,13 ms | 839,16 ms | 1.309,90 ms | 0% | **không đạt** | đạt |

Commit `ec8af67` chỉ thêm `BUYER_MODE` vào script k6; code booking giống hệt `e1c5b98`. Summary JSON gốc của k6: [`docs/results/`](results/).

### Nhận xét

- **Chế độ `vu` không đo được năng lực server.** Người mua đến theo tốc độ ramp (tối đa khoảng 200 người mới mỗi giây) và dừng sau khi có đơn, nên RPS hold bị giới hạn bởi kịch bản chứ không phải bởi booking. Ở chế độ này còn dư 344 và 307 ghế sau hai lần chạy.
- **Cùng code, p99 lệch nhau 5 lần** giữa lần 1 (175 ms) và lần 2 (35,5 ms). Đo trên laptop, với k6 chạy cùng máy, rất nhiễu. Kết luận về latency đuôi cần nhiều lần chạy (M7).
- **Chế độ `iteration` bán hết 5.000 ghế trong khoảng 11 giây.** Trong lúc đó booking xử lý khoảng 3.000–3.500 request/giây mọi loại (login, sơ đồ ghế, giữ ghế). p99 của API giữ ghế lên 839 ms, không đạt ngưỡng 200 ms. Sau khi hết ghế, k6 vẫn gửi login và tải sơ đồ ghế đến hết 60 giây, nên tổng cộng có 158.292 request.
- **Chưa phân tích điểm nghẽn** (việc của M7). Giả thuyết cần kiểm chứng: pool 20 kết nối PostgreSQL bị chia sẻ với dev-login (ở chế độ `iteration`, mỗi vòng lặp là một `INSERT users`), và các request chờ khoá hàng trên những ghế VIP bị tranh nhiều nhất.
- **Chưa lần nào đạt mục tiêu ≥ 5.000 req/s** cho API giữ ghế. Kịch bản này cũng không được thiết kế để tìm throughput tối đa.

## So sánh PG và Redis

Chưa đo (M2).

## Tìm và sửa điểm nghẽn

Chưa đo (M7).

## Chaos

Chưa đo (M8).
