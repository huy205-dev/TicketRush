# ADR-006: Các lựa chọn ở M1 khi SPEC chưa nói rõ

- Trạng thái: Chấp nhận (mục 1 được chốt khi review M1)
- Ngày: 2026-09-25

## Bối cảnh

Khi làm M1, có một số chỗ SPEC không quy định hoặc có thể hiểu theo nhiều cách. CLAUDE.md yêu cầu những lựa chọn bắt buộc phải đưa ra thì ghi vào ADR. Từng mục dưới đây nêu lựa chọn và lý do; mục nào sau này được quyết định khác thì cập nhật tại đây.

## Quyết định

### 1. Cách hiểu kịch bản `hold_contention.js` (đã chốt: `iteration`)

Câu "Mỗi VU: dev-login, xem ghế, chọn 1–4 ghế trống, tạo đơn, 409 thì thử lại tối đa 3 lần" có hai cách hiểu. Script hỗ trợ cả hai qua biến `BUYER_MODE`:

- `iteration` (mặc định, **chuẩn so sánh**): mỗi vòng lặp là một người mua mới (login mới), VU đặt liên tục đến khi hết ghế.
- `vu`: mỗi VU là một người mua; có đơn thì dừng. Mỗi người chỉ được giữ một đơn cho mỗi sự kiện, nên nếu cùng user tiếp tục đặt sẽ gặp `ACTIVE_ORDER_EXISTS`.

Chế độ `vu` bị giới hạn bởi tốc độ ramp, nên không làm server bão hoà được. Chế độ `iteration` dồn tải lên API giữ ghế nhiều hơn khoảng 4 lần.

Review M1 đã chốt `iteration` làm chuẩn so sánh giữa các backend. Quy trình đo chuẩn là `make bench-hold BACKEND=<backend>`:
- chạy 3 lần;
- trước mỗi lần reset DB, flush Redis và seed lại;
- báo cáo trung vị kèm min/max.

Chế độ `vu` được giữ lại để kiểm tra hành vi của người mua thật, không dùng để so sánh.

### 2. Sơ đồ ghế của sự kiện mẫu

VIP có 10 hàng × 50 ghế, CAT1 có 15 hàng × 100 ghế, CAT2 có 20 hàng × 150 ghế. Hàng đánh chữ `A, B, …`, ghế đánh số từ 1, mã ghế dạng `VIP-A-1`. SPEC chỉ quy định số ghế và giá của từng khu.

### 3. Tính năng của milestone sau

- `INVENTORY_BACKEND` mặc định là `redis` và `REQUIRE_ADMISSION` mặc định là `true` (theo SPEC mục 11), nhưng hai tính năng này chỉ có từ M2 và M5.
- Nếu gặp hai giá trị đó, booking từ chối khởi động kèm thông báo rõ ràng, thay vì âm thầm chạy mà không có chúng.
- `.env.example` đặt tạm `INVENTORY_BACKEND=pg` và `REQUIRE_ADMISSION=false`.
- *Cập nhật M2:* backend Redis đã có. Booking nhận cả hai giá trị, mặc định là `redis`. Chỉ còn `REQUIRE_ADMISSION=true` bị từ chối cho đến M5.

### 4. Quy ước API

- Xem hoặc huỷ đơn của người khác trả `404 ORDER_NOT_FOUND`, không phải 403, để không lộ việc đơn đó có tồn tại.
- Huỷ đơn ở trạng thái khác `HELD`/`CANCELLED` trả `409 ORDER_NOT_CANCELLABLE`, kèm `details.status`.
- Body không phải JSON, có field lạ hoặc có dữ liệu thừa sau object trả `400 BAD_REQUEST`. Lỗi về nội dung (sai số ghế, ghế không tồn tại, chưa mở bán, thiếu Idempotency-Key) trả `422 VALIDATION_ERROR`, kèm `details.field` và `details.reason`.
- Sự kiện không tồn tại trả `404 EVENT_NOT_FOUND` khi gọi `GET /v1/events/{id}`, và `422 VALIDATION_ERROR` (field `event_id`) khi gọi `POST /v1/orders`.
- `GET /v1/events/{id}` trả về: `id`, `name`, `venue`, `starts_at`, `sale_opens_at`, `max_seats_per_order`, `zones[] {zone, price_vnd, seats}`.
- Mọi thời gian trả về theo UTC, định dạng RFC 3339.
- `payment_url` chưa xuất hiện trong response cho đến M4, khi đã có fakepay.
- Sơ đồ ghế được nén gzip khi client gửi `Accept-Encoding: gzip`: khoảng 485 KB còn 30 KB.

### 5. Outbox từ M1

Việc tạo đơn và huỷ đơn đã ghi `order.held` và `order.cancelled` vào outbox trong cùng transaction. Làm vậy để tuân thủ quy tắc trong CLAUDE.md ngay từ đầu. Relay đẩy dữ liệu lên Kafka sẽ có ở M3.

### 6. Seed

- `cmd/seed` tạo một sự kiện mới ở mỗi lần chạy và in ra id.
- Cần database sạch (sự kiện có id 1) thì chạy `make db-reset` (goose reset + up) trước.
- Thời điểm mở bán và bắt đầu tính theo `now()` của PostgreSQL cộng với độ lệch, đặt qua các cờ `-sale-opens-in` và `-starts-in`.

## Hệ quả

- Hành vi của API có test cho từng mục (`cmd/booking/api_integration_test.go`).
- Mục 1 quyết định số liệu so sánh ở M2; mọi phép so sánh backend đều dùng `make bench-hold`.
