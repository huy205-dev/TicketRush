# Kết quả đo

Chỉ ghi kết quả từ lần chạy thật. Mỗi lần đo ghi: ngày, commit hash, cấu hình máy, kịch bản, RPS, p50/p95/p99, tỉ lệ lỗi, kết quả invariants, ghi chú thay đổi so với lần trước.

Số liệu chạy k6 cùng máy với hệ thống chỉ dùng để so sánh tương đối (k6 tranh CPU với service). Số liệu chính thức chạy trên VPS riêng.

## Quy trình đo chuẩn

Chạy `make bench-hold BACKEND=<pg|redis>` (script [`loadtest/bench_hold.sh`](../loadtest/bench_hold.sh)). Script này:

1. Build `booking` và `seed` từ commit hiện tại. Nếu working tree có thay đổi chưa commit, kết quả được gắn nhãn `-dirty`.
2. Lặp 3 lần (đổi bằng `RUNS=`). Mỗi lần:
   - `goose reset` + `goose up`, `FLUSHALL` Redis, seed sự kiện mẫu;
   - khởi động một process booking mới với `INVENTORY_BACKEND=<backend>`;
   - chạy [`loadtest/hold_contention.js`](../loadtest/hold_contention.js) với `BUYER_MODE=iteration` và tham số mặc định (2.000 VU, ramp 10 s, giữ 60 s);
   - dừng booking, rồi kiểm tra dữ liệu bằng [`loadtest/sql/`](../loadtest/sql/);
   - nghỉ 5 s trước lần kế tiếp.
3. Báo cáo **trung vị** kèm **min** và **max** của từng chỉ số, cùng bảng chi tiết từng lần. Kết quả nằm trong `loadtest/out/bench-hold-<backend>-<thời điểm UTC>/summary.md`.

`BUYER_MODE=iteration` là chế độ chuẩn để so sánh giữa các backend (được chốt khi review M1, xem [ADR-006](adr/006-lua-chon-khi-spec-chua-ro-m1.md)): mỗi vòng lặp là một người mua mới, đặt liên tục đến khi hết ghế.

**Cách tính**
- *RPS giữ ghế đỉnh*: số request `POST /v1/orders` trong giây đông nhất, tính bằng [`loadtest/peak_rps.sh`](../loadtest/peak_rps.sh) từ CSV của k6.
- *RPS giữ ghế TB*: tổng request giữ ghế chia cho số giây có request giữ ghế.
- Latency: `http_req_duration{name:hold}` của k6.
- Tỉ lệ lỗi: `http_req_failed{name:hold}`; 409 được tính là phản hồi hợp lệ (ghế đã có người giữ là kết quả nghiệp vụ bình thường).
- Kiểm tra dữ liệu (đến M7 sẽ thay bằng `tools/invariants`):
  - không ghế nào thuộc hai đơn `HELD`;
  - tổng tiền mỗi đơn bằng tổng giá ghế;
  - đơn nào cũng có sự kiện `order.held` trong outbox;
  - riêng backend pg: `seat_holds` còn hạn khớp 1–1 với ghế của các đơn `HELD`.

## Baseline PG

Backend `INVENTORY_BACKEND=pg`: giữ ghế bằng `SELECT … FOR UPDATE` trên bảng `seats` rồi ghi `seat_holds` (xem [ADR-000](adr/000-ban-goc-postgresql.md)).

### Cấu hình

| | |
|---|---|
| Máy | MacBook, Apple M5 Pro (15 core: 5 performance + 10 efficiency), 24 GiB RAM, macOS 26.4 |
| Container | OrbStack 2.2.3, VM 15 CPU / 11,7 GiB |
| PostgreSQL | 16.15 (image `postgres:16.15-alpine`, cấu hình mặc định: `shared_buffers=128MB`, `max_connections=100`, `synchronous_commit=on`) |
| Booking | Binary build từ commit, chạy thẳng trên macOS (không trong container). `DB_MAX_CONNS=20`, `HOLD_TTL=10m`, `HOLD_GRACE=30s`, log JSON ghi ra file |
| Công cụ | Go 1.27.1, k6 v2.3.0 chạy cùng máy |

### Kết quả chuẩn: 2026-09-25, commit `99d3e6d`, `make bench-hold BACKEND=pg`

Code booking ở commit này giống hệt `e1c5b98`; các commit ở giữa chỉ đổi script đo, test và tài liệu.

| Chỉ số | Trung vị | Min | Max |
|---|---|---|---|
| Request giữ ghế | 14.776 | 14.275 | 14.918 |
| 201 (đơn tạo được) | 2.439 | 2.431 | 2.444 |
| 409 SEATS_UNAVAILABLE | 12.337 | 11.844 | 12.474 |
| RPS giữ ghế đỉnh | 2.124 req/s | 2.086 | 2.184 |
| RPS giữ ghế TB trên giây có tải | 1.190 req/s | 924 | 1.243 |
| p50 | 399,1 ms | 383,2 | 503,3 |
| p95 | 803,8 ms | 766,1 | 1.342,5 |
| p99 | 970,8 ms | 875,4 | 1.503,5 |
| max | 2.392,4 ms | 2.016,6 | 4.705,4 |
| Tỉ lệ lỗi | 0% | 0% | 0% |

- Ngưỡng p99 < 200 ms: **không đạt** ở cả 3/3 lần.
- Kiểm tra dữ liệu: không vi phạm ở 3/3 lần. Log booking không có dòng `ERROR` nào.

| Lần | Request | 201 / 409 | Đỉnh (req/s) | p50 (ms) | p95 (ms) | p99 (ms) | Lỗi | Vi phạm |
|---|---|---|---|---|---|---|---|---|
| 1 | 14.776 | 2.439 / 12.337 | 2.124 | 503,35 | 1.342,47 | 1.503,52 | 0% | 0 |
| 2 | 14.918 | 2.444 / 12.474 | 2.184 | 383,16 | 766,12 | 970,76 | 0% | 0 |
| 3 | 14.275 | 2.431 / 11.844 | 2.086 | 399,12 | 803,77 | 875,39 | 0% | 0 |

Dữ liệu thô (summary của k6, kết quả kiểm tra từng lần): [`docs/results/2026-09-25-m1-pg-bench-hold/`](results/2026-09-25-m1-pg-bench-hold/).

### Nhận xét

- **Throughput ổn định, latency đuôi thì không.** RPS đỉnh chỉ lệch ±3% giữa các lần. p99 lệch từ 875 đến 1.504 ms, và riêng lần 1 chậm hơn rõ ở mọi phân vị. Vì vậy mọi so sánh đều dùng trung vị của 3 lần.
- **Mỗi lần bán hết 5.000 ghế trong 12–16 giây** (lần 2 và 3 mất 12 giây, lần 1 chậm nhất mất 16 giây), với khoảng 2.440 đơn (trung bình khoảng 2 ghế mỗi đơn). Cứ 6 request giữ ghế thì khoảng 5 request nhận 409, vì các buyer tranh nhau những ghế còn trống cuối cùng.
- **Chưa đạt mục tiêu** ≥ 5.000 req/s và p99 < 200 ms. Đây là con số để bản Redis ở M2 so sánh.
- **Chưa phân tích điểm nghẽn** (việc của M7). Giả thuyết cần kiểm chứng: pool 20 kết nối PostgreSQL bị chia sẻ với dev-login (mỗi vòng lặp là một `INSERT users`), và các request chờ khoá hàng trên những ghế bị tranh nhiều nhất.

### Lịch sử: đo đơn lẻ trước review M1 (quy trình cũ)

Mỗi cấu hình chỉ chạy một lần, DB được reset trước mỗi lần, và vẫn dùng chung cấu hình máy như trên. Các số liệu này được giữ lại để tham khảo; **không dùng để so sánh**, vì chỉ có một lần đo và `vu` không còn là chế độ chuẩn.

| Lần | Ngày | Commit | BUYER_MODE | Request giữ ghế | 201 / 409 | Đỉnh (req/s) | p50 | p95 | p99 | Lỗi |
|---|---|---|---|---|---|---|---|---|---|---|
| A | 2026-09-25 | `e1c5b98` | vu | 3.728 | 2.000 / 1.728 | 888 | 3,13 ms | 35,40 ms | 175,19 ms | 0% |
| B | 2026-09-25 | `ec8af67` | vu | 3.901 | 2.000 / 1.901 | 765 | 3,04 ms | 17,01 ms | 35,52 ms | 0% |
| C | 2026-09-25 | `ec8af67` | iteration | 14.825 | 2.447 / 12.378 | 2.209 | 347,99 ms | 790,13 ms | 839,16 ms | 0% |

- Ở chế độ `vu`, người mua đến theo tốc độ ramp (tối đa khoảng 200 người mới mỗi giây) và dừng sau khi có đơn, nên RPS bị giới hạn bởi kịch bản chứ không phải bởi server.
- Lần A và B chạy cùng code nhưng p99 lệch nhau 5 lần. Đây chính là lý do chuyển sang đo 3 lần và báo cáo trung vị.
- Dữ liệu thô: [`docs/results/2026-09-25-m1-pg-single-runs/`](results/2026-09-25-m1-pg-single-runs/).

## So sánh PG và Redis

Chưa đo (M2). Sẽ dùng `make bench-hold BACKEND=redis` trên cùng máy và cùng cấu hình.

## Tìm và sửa điểm nghẽn

Chưa đo (M7).

## Chaos

Chưa đo (M8).
