# ADR-004: Cách chọn `ADMIT_BATCH`

- Trạng thái: Chấp nhận
- Ngày: 2026-11-04

## Bối cảnh

Phòng chờ (SPEC 7.2) cứ mỗi `ADMIT_INTERVAL` (mặc định 1 s) lại cho `ADMIT_BATCH` người đầu hàng vào, tức là cấp cho mỗi người một admission token. Chỉ người có token mới gọi được `POST /v1/orders` (`REQUIRE_ADMISSION=true`). `ADMIT_BATCH` vì vậy là cái van điều chỉnh tải đổ vào booking:

- **Quá lớn:** hàng nghìn người cùng giành ghế. Đây đúng là kịch bản `hold_contention.js` đã đo: phần lớn request nhận `409`, và latency 409 ở p99 lên khoảng 200 ms. Phòng chờ khi đó mất tác dụng.
- **Quá nhỏ:** booking rảnh trong khi người mua chờ lâu, và vé bán chậm hơn mức hệ thống làm được.

SPEC yêu cầu giá trị ban đầu phải tính từ throughput đo được của booking, và ghi lại cách tính.

## Các phương án đã cân nhắc

1. **Giữ nguyên 200 của SPEC mà không kiểm chứng.** Không đạt yêu cầu "tính từ throughput đo được".
2. **Điều chỉnh tự động theo tải** (đo latency hoặc tỉ lệ 409 của booking rồi tăng giảm batch). Tốt hơn về lâu dài, nhưng cần metric (M6) và một vòng điều khiển cần được kiểm chứng riêng. Để sau.
3. **Tính một giá trị cố định từ số đo hiện có, có biên an toàn, và đo lại ở M7.** Chọn phương án này.

## Quyết định

**`ADMIT_BATCH = 200`, `ADMIT_INTERVAL = 1s`** (đúng giá trị mặc định của SPEC), cho backend Redis. Với backend PG thì nên đặt **150**.

### Số đo dùng để tính

Lấy từ [docs/results.md, mục "Kịch bản ETag"](../results.md#kịch-bản-etag-đo-lại-pg-và-redis), commit `3e31df4`, MacBook Apple M5 Pro 24 GiB, `DB_MAX_CONNS=20`, đo 3 lần mỗi backend, lấy trung vị [min–max]:

| | Redis | PG |
|---|---|---|
| `POST /v1/orders` trung bình trên các giây có tải (request/s) | 2.952 [2.484–3.032] | 1.434 [1.414–1.437] |
| `POST /v1/orders` đỉnh (request/s) | 4.038 [3.733–4.186] | 2.048 [2.044–2.244] |
| 201 / tổng request mỗi lần đo | 2.446 / 14.901 | 2.430 / 15.768 |
| 201 p99 phía server lúc bão hoà | 237,6 ms | 1.066,2 ms |

Số đo này là lúc **bão hoà**: 2.000 VU cùng giành ghế. Vì vậy C (năng lực xử lý) lấy theo số trung bình của các giây có tải, không lấy số đỉnh.

### Công thức

Mỗi người được cho vào sẽ tạo đơn; nếu bị `409` thì thử lại, tối đa 3 lần (giống `hold_contention.js`). Trường hợp xấu nhất, một người gửi **R = 4** request tạo đơn ngay trong chu kỳ đầu sau khi vào. Muốn booking chạy ở mức **U = 50%** năng lực đo được, để latency còn xa ngưỡng bão hoà:

```
ADMIT_BATCH ≤ U × C × ADMIT_INTERVAL / R
```

- Redis: 0,5 × 2.952 × 1 / 4 = **369**. Nếu lấy lần đo kém nhất (2.484) thì được **310**.
- PG: 0,5 × 1.434 × 1 / 4 = **179**.

Với `ADMIT_BATCH = 200`, tải xấu nhất là 800 request/s. Con số này bằng **27%** năng lực trung bình đo được của Redis (32% nếu so với lần đo kém nhất), nên còn dư để hấp thụ các đợt dồn. Với PG thì 800 request/s đã là **56%**, vượt mức U, nên dùng 150 (tải xấu nhất 600 request/s, 42%).

Kiểm tra chéo bằng số đơn tạo được: lúc bão hoà, backend Redis tạo khoảng 485 đơn 201/s. Số này tính từ trung vị: 2.446 đơn trên 14.901 request, ở 2.952 request/s. Cho 200 người vào mỗi giây, mỗi người một đơn, thì cần khoảng 41% mức đó.

### Worker có theo kịp không

Đo bằng `go test -tags=integration -run '^$' -bench Admit -benchtime=100x -count=5 ./internal/queue`. Commit `3852f07`, Apple M5 Pro, Redis 7.4.11 trong container (OrbStack, VM 15 CPU). Một lượt cấp 200 token (đọc đầu hàng, ký 200 JWT, ghi `admitted:*`, xoá khỏi hàng) mất trung vị **1,448 ms** [1,439–1,476]. Con số này chưa tới 0,15% của chu kỳ 1 s, nên worker không giới hạn `ADMIT_BATCH`.

## Hệ quả

- **Thời gian bán hết:**
  - Sự kiện mẫu có 5.000 ghế; mỗi đơn chọn 1–4 ghế, trung bình 2,5, nên cần khoảng 2.000 đơn.
  - Với 200 người mỗi giây, nếu ai vào cũng mua thì sau khoảng 10 s phòng chờ đã cho đủ người để giành hết ghế. Sau đó còn thêm thời gian người mua chọn ghế và thanh toán.
  - Người chờ ở vị trí N đợi khoảng ⌈N / 200⌉ giây. Stream SSE trả `estimated_wait_seconds` theo đúng công thức này.
- **Người được cho vào nhưng không mua** (đóng tab, chọn lâu) làm phí một lượt. Lượt đó chỉ hết khi token hết hạn (`ADMISSION_TTL` 5 phút). Nếu tỉ lệ này cao, có thể tăng `ADMIT_BATCH`, vì tải thật sẽ thấp hơn tải xấu nhất ở trên.
- **Chưa đo latency ở mức 27% tải.** Lập luận "còn xa ngưỡng bão hoà" là suy ra từ số đo lúc bão hoà, chưa có số đo trực tiếp. Kịch bản `full_flow.js` (M7, chạy trên VPS) sẽ đo booking dưới tải đi qua phòng chờ, và giá trị này sẽ được xem lại theo kết quả đó.
- **Số đo lấy trên laptop**, và k6 chạy cùng máy (xem quy ước báo cáo ở `docs/results.md`). Trên VPS, C sẽ khác; công thức giữ nguyên, chỉ thay số.
- **Giá trị được cấu hình qua biến môi trường** (`ADMIT_BATCH`, `ADMIT_INTERVAL`), nên chỉnh không cần build lại.
