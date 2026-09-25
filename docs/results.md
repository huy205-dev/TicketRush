# Kết quả đo

Chỉ ghi kết quả từ lần chạy thật. Mỗi lần đo ghi: ngày, commit hash, cấu hình máy, kịch bản, RPS, p50/p95/p99, tỉ lệ lỗi, kết quả invariants, ghi chú thay đổi so với lần trước.

Số liệu chạy k6 cùng máy với hệ thống chỉ dùng để so sánh tương đối (k6 tranh CPU với service). Số liệu chính thức chạy trên VPS riêng.

## Quy trình đo chuẩn

Chạy `make bench-hold BACKEND=<pg|redis>` (script [`loadtest/bench_hold.sh`](../loadtest/bench_hold.sh)). Script này:

1. Build `booking` và `seed` từ commit hiện tại. Nếu working tree có thay đổi chưa commit, kết quả được gắn nhãn `-dirty`.
2. Lặp 3 lần (đổi bằng `RUNS=`). Mỗi lần:
   - `goose reset` + `goose up`, `FLUSHALL` Redis, seed sự kiện mẫu;
   - khởi động một process booking mới với `INVENTORY_BACKEND=<backend>`;
   - (từ điều tra M2) tạo sẵn token bằng dev-login ([`loadtest/make_tokens.js`](../loadtest/make_tokens.js)), để việc đăng nhập nằm ngoài tải đo;
   - chạy [`loadtest/hold_contention.js`](../loadtest/hold_contention.js) với `BUYER_MODE=iteration` và tham số mặc định (2.000 VU, ramp 10 s, giữ 60 s);
   - dừng booking, rồi kiểm tra dữ liệu bằng [`loadtest/sql/`](../loadtest/sql/);
   - (từ điều tra M2) tách latency phía server từ log booking, ghi CPU mà k6, booking và cả máy dùng, và loại lần chạy nếu có container hạ tầng bị khởi động lại;
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
- **Chưa đạt mục tiêu** ≥ 5.000 req/s và p99 < 200 ms. Phép so sánh với Redis ở M2 dùng số PG đo lại ở commit M2 (mục "So sánh PG và Redis"), không dùng bảng này.
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

2026-09-25, commit `0cc0197` (M2). Cả hai backend đều đo bằng `make bench-hold`: 3 lần mỗi backend, liên tiếp nhau trên cùng máy và cùng cấu hình như mục Baseline PG. PG được đo lại ở chính commit này, vì từ M2 mọi request tạo đơn đều đi qua khoá idempotency trong Redis (ADR-005), nên cả hai bên cùng chạy một phiên bản code.

| Chỉ số (trung vị [min–max]) | PG | Redis | Thay đổi (trung vị) |
|---|---|---|---|
| Request giữ ghế | 14.652 [14.466–15.311] | 13.595 [13.509–14.541] | |
| 201 (đơn tạo được) | 2.421 [2.410–2.435] | 2.430 [2.389–2.433] | |
| RPS giữ ghế đỉnh | 2.070 [1.762–2.154] | 2.901 [2.775–2.971] | **+40%** |
| RPS giữ ghế TB trên giây có tải | 1.178 [1.033–1.221] | 1.351 [1.212–1.511] | +15% |
| p50 | 461,7 ms [446,2–489,3] | 47,7 ms [44,0–63,1] | **−90%** |
| p95 | 973,6 ms [728,5–1.035,0] | 502,2 ms [454,2–542,1] | −48% |
| p99 | 1.061,5 ms [959,3–1.115,5] | 958,1 ms [764,5–1.099,7] | −10% |
| max | 2.205,1 ms [1.801,1–3.047,8] | 2.197,6 ms [2.032,7–2.286,2] | ≈ 0 |
| Tỉ lệ lỗi | 0% | 0% | |
| Ngưỡng p99 < 200 ms | 0/3 lần | 0/3 lần | |
| Kiểm tra dữ liệu | 3/3 lần đạt | 3/3 lần đạt (kèm so khớp Redis–PostgreSQL: 5.000/5.000 key đúng) | |

Dữ liệu thô: [`docs/results/2026-09-25-m2-pg-bench-hold/`](results/2026-09-25-m2-pg-bench-hold/), [`docs/results/2026-09-25-m2-redis-bench-hold/`](results/2026-09-25-m2-redis-bench-hold/).

### Nhận xét

- **Redis thắng rõ ở phần thân của phân bố latency và ở throughput.**
  - Trung vị latency giảm khoảng 10 lần.
  - RPS đỉnh tăng 40%.
  - Khoảng min–max của hai backend không chồng lên nhau ở RPS đỉnh, p50 và p95, nên đây không phải nhiễu.
- **Đuôi p99 gần như không đổi**: −10%, và khoảng của hai bên chồng lên nhau. Với Redis, bước giữ ghế không còn chạm PostgreSQL, nhưng mỗi request tạo đơn vẫn phải qua PostgreSQL ít nhất một lần:
  - tra Idempotency-Key (bước 4 của SPEC 9.1);
  - với đơn giữ được ghế, thêm transaction ghi đơn;
  - ngoài ra mỗi vòng lặp của k6 còn gọi dev-login, tức là một câu `INSERT users`.

  Tất cả dùng chung pool 20 kết nối. Giả thuyết là đuôi latency đến từ việc chờ kết nối trong pool này và từ việc k6 tranh CPU với booking trên cùng máy. *Cập nhật:* [Điều tra M2](#điều-tra-m2) bác bỏ vế đầu (tăng pool lên 80 xoá hết thời gian chờ mà p99 không đổi) và xác nhận vế sau (k6 dùng khoảng 10/15 core, booking khoảng 0,25 core).
- **Vẫn chưa đạt mục tiêu** ≥ 5.000 req/s và p99 < 200 ms ở cả hai backend.
- **Chi phí của khoá idempotency** lên backend PG, so với baseline M1 (commit `99d3e6d`): RPS đỉnh 2.070 so với 2.124, p50 462 ms so với 399 ms, p99 1.062 ms so với 971 ms. Mức chênh này nằm trong độ dao động quan sát được giữa các lần đo, nên chưa thể tách riêng ảnh hưởng của khoá.

## Điều tra M2

Ở M2, Redis cải thiện RPS đỉnh và p50, nhưng p99 phía k6 vẫn quanh 1 s. Review M2 yêu cầu **đo để tìm nguyên nhân trước khi sửa** (nhánh `perf/m2-investigation`). Mọi con số dưới đây là backend Redis, `make bench-hold` 3 lần cho mỗi cấu hình, trung vị [min–max], cùng máy với các mục trên.

### Thay đổi cách đo

- **dev-login ra khỏi tải đo.**
  - `loadtest/make_tokens.js` gọi dev-login theo lô trong `setup()` và ghi token ra file; `hold_contention.js` đọc file đó vào một `SharedArray`.
  - Không đặt dev-login trong `setup()` của chính `hold_contention.js`, vì k6 chép dữ liệu trả về từ `setup()` vào từng VU: 50.000 token × 2.000 VU sẽ tốn hàng chục GB.
  - Mỗi VU dùng một dãy 25 token riêng, và chuyển sang token mới sau mỗi đơn tạo được.
- **Latency tách theo kết quả** phía k6: `201`, `409 SEATS_UNAVAILABLE`, lỗi khác.
- **Phía server:**
  - tracer của pgx và hook của go-redis ghi vào access log thời gian chờ lấy kết nối pgxpool (`db_acquire_ms`), thời gian chạy query PostgreSQL (`db_query_ms`) và thời gian gọi Redis (`redis_ms`) của từng request;
  - booking log thống kê pgxpool mỗi giây;
  - [`loadtest/analyze_booking_log.sh`](../loadtest/analyze_booking_log.sh) tính phân vị cho `POST /v1/orders` theo từng status.
- **CPU:** thời gian CPU k6 và booking dùng (chia cho thời gian thực, ra số core trung bình), và % CPU bận của cả máy theo từng giây.
- **Chốt chặn:** lần chạy nào có container hạ tầng bị khởi động lại giữa chừng thì bị loại. Một lần như vậy đã xảy ra lúc 11:34:15 UTC: toàn bộ stack compose bị `kill`/`start` từ bên ngoài phiên đo. Số liệu của lần đó không được dùng.

### Kết quả theo `DB_MAX_CONNS`

Commit `715259d`, `VUS=2000`, chỉ đổi `DB_MAX_CONNS`.

| Chỉ số | 20 | 40 | 80 |
|---|---|---|---|
| RPS giữ ghế đỉnh | 3.414 [3.239–3.485] | 2.568 [2.463–2.784] | 2.654 [2.446–2.743] |
| **k6: p99 tất cả** | 1.102 ms [833–1.466] | 882 ms [755–1.198] | 1.057 ms [908–1.667] |
| k6: p50 tất cả | 32,8 ms [25,1–39,5] | 55,3 ms [44,0–84,5] | 49,1 ms [31,0–54,1] |
| k6: p99 request thắng (201) | 849 ms [646–953] | 735 ms [720–749] | 869 ms [847–1.228] |
| k6: p99 request thua (409) | 1.135 ms [821–1.549] | 905 ms [756–1.251] | 1.063 ms [918–1.750] |
| **Server: p50 / p99 của 409** | 1,38 / 24,8 ms | 1,82 / 207,0 ms | 1,72 / 28,8 ms |
| **Server: p99 chờ pgxpool của 409** | **10,5 ms** [8,8–16,7] | 3,4 ms [2,9–26,1] | **0,03 ms** [0,02–1,08] |
| Server: tỉ lệ thời gian 409 chờ pgxpool | 16% | 2% | 1% |
| Server: p99 query PG / p99 Redis của 409 | 3,6 / 13,1 ms | 5,3 / 203,8 ms | 5,5 / 22,5 ms |
| Server: p50 / p99 của 201 | 12,6 / 54,8 ms | 17,7 / 87,1 ms | 16,6 / 108,6 ms |
| Lần acquire phải chờ vì pool rỗng (mỗi lần chạy) | 2.306 | 505 | 113 |
| **CPU k6 / booking (số core)** | **10,7 / 0,25** | 10,0 / 0,27 | 10,2 / 0,27 |
| CPU cả máy (15 core): TB / p90 theo giây | 79% / 83% | 79% / 86% | 79% / 82% |
| Lỗi / kiểm tra dữ liệu | 0% / 3/3 đạt | 0% / 3/3 đạt | 0% / 3/3 đạt |

**Chẩn đoán thêm**, `VUS=1000`, `DB_MAX_CONNS=20` (không phải phép đo chuẩn):
- p99 phía k6 **375 ms** [338–415], p50 20,3 ms, RPS đỉnh 3.109;
- phía server: p99 của 409 là 14,8 ms, p99 của 201 là 45,8 ms;
- CPU: k6 9,9 core, booking 0,23 core, cả máy 74%.

Lần chạy này có 28 lần VU dùng hết 25 token (với `VUS=2000` là 0–1 lần), nên tải cuối lần chạy bị cắt bớt một chút.

Dữ liệu thô: [`docs/results/2026-09-25-m2-investigation/`](results/2026-09-25-m2-investigation/). Thư mục `redis-conns20-no-cpu` là một lần đo 20 kết nối ở commit `f253412`, trước khi có phần đo CPU; kết quả tương tự (RPS đỉnh 3.016, p99 1.037 ms).

### Trước và sau khi đổi cách đo

Cùng backend Redis, `DB_MAX_CONNS=20`, trung vị của 3 lần:

| | M2, dev-login nằm trong tải (`0cc0197`) | Sau khi đổi cách đo (`715259d`) |
|---|---|---|
| RPS giữ ghế đỉnh | 2.901 | 3.414 (+18%) |
| p50 | 47,7 ms | 32,8 ms |
| p95 | 502,2 ms | 483,2 ms |
| p99 | 958,1 ms | 1.102,4 ms |

Bỏ dev-login khỏi tải làm tăng RPS đỉnh và giảm p50, nhưng không giảm p99.

### Kết luận

1. **Request thua có chờ PostgreSQL, nhưng đó không phải nguyên nhân của đuôi khoảng 1 s.**
   - Mỗi request 409 vẫn tra Idempotency-Key trong PostgreSQL. Với 20 kết nối, thời gian chờ pgxpool ở p99 là 10,5 ms, bằng 16% thời gian xử lý phía server.
   - Tăng lên 80 kết nối thì thời gian chờ này gần như biến mất (p99 0,03 ms, số lần acquire phải chờ giảm từ 2.306 xuống 113), nhưng p99 phía k6 không đổi: 1.102 → 882 → 1.057 ms, các khoảng min–max chồng lên nhau.
2. **Đuôi latency nằm ngoài booking.**
   - Phía server, p99 của 409 là 15–30 ms và của 201 là 46–109 ms. Riêng cấu hình 40 kết nối có 2/3 lần mà `redis_ms` của 409 lên khoảng 200 ms; nguyên nhân chưa rõ.
   - Phía k6, p99 khoảng 900–1.100 ms.
   - Booking chỉ dùng khoảng 0,25 core, trong khi k6 dùng khoảng 10 trên 15 core và cả máy bận khoảng 79% (p90 83–86%).
3. **Máy tạo tải bị bão hoà.**
   - Giảm còn 1.000 VU thì p99 phía k6 giảm khoảng 3 lần (1.102 → 375 ms), trong khi latency phía server gần như không đổi.
   - Nguồn tốn CPU của k6: mỗi vòng lặp tải và parse sơ đồ ghế 485 KB bằng JavaScript, trung bình khoảng 1.000 lần/s (đỉnh khoảng 1.500 lần/s), tức khoảng 500 MB JSON mỗi giây.
4. **Chưa làm bước 2** (sửa luồng SPEC 9.1 để request thua không chạm PostgreSQL), vì số liệu không xác nhận điều kiện đặt ra. Nếu làm, theo số trên thì phía server chỉ bớt được khoảng 1 truy vấn cho mỗi request thua (p99 chờ pool khoảng 10 ms với 20 kết nối), không đủ để thay đổi p99 phía k6 trong cách đo hiện tại.

Quan sát phụ, chưa kết luận: Redis lưu snapshot RDB (`save 60 10000` mặc định) trong mỗi lần đo, mỗi lần fork khoảng 15–20 ms.

## Outbox relay

2026-09-25, commit `e18f5ce` (M3), `make bench-relay` (script [`loadtest/bench_relay.sh`](../loadtest/bench_relay.sh)). Mỗi lần chạy:
- reset DB rồi chèn thẳng 200.000 dòng outbox, mỗi dòng có payload JSON khoảng 250 byte giống sự kiện đơn hàng thật;
- khởi động relay và đo thời gian đến khi không còn dòng nào có `published_at IS NULL`, kiểm tra mỗi 100 ms.

Thời gian đo tính cả lúc relay khởi động (kết nối, kiểm tra topic) và độ trễ tối đa 100 ms của bước kiểm tra, nên con số là cận dưới.

- Máy như mục Baseline PG. Redpanda v26.2.3 trong compose: `--smp 1 --memory 1G --mode dev-container`; chế độ này bỏ qua fsync, nên throughput cao hơn một cụm thật.
- Relay: lô 500 dòng, `ProduceSync` với producer idempotent, `acks=all`.

| Lần | Thời gian | Dòng/s |
|---|---|---|
| 1 | 2,449 s | 81.682 |
| 2 | 1,979 s | 101.085 |
| 3 | 1,887 s | 106.001 |
| **Trung vị [min–max]** | | **101.085** [81.682–106.001] |

- Tổng high watermark của `orders.v1` tăng đúng 600.000 sau 3 lần, tức mọi dòng được đánh dấu đã gửi đều thực sự nằm trên Kafka.
- Lần 1 chậm nhất (khởi động nguội).
- Throughput này cao hơn nhu cầu nhiều bậc: khi mở bán, backend Redis tạo khoảng 2.400 đơn trong 12–16 giây, mỗi đơn một sự kiện `order.held`.

Dữ liệu thô: [`docs/results/2026-09-25-m3-relay/`](results/2026-09-25-m3-relay/).

## Tìm và sửa điểm nghẽn

Chưa đo (M7).

## Chaos

Chưa đo (M8).
