# ADR-005: Idempotency-Key cho API tạo đơn

- Trạng thái: Chấp nhận. Phần "request cùng key đang xử lý" đã được sửa sau review M1 và sẽ làm ở M2 (xem mục cuối).
- Ngày: 2026-09-25

## Bối cảnh

Client gọi lại `POST /v1/orders` khi mạng chập chờn hoặc người dùng bấm hai lần. SPEC mục 7.1 và 9.1 quy định:
- Cùng key và cùng body thì trả `200` với đơn cũ.
- Cùng key nhưng body khác thì trả `422 IDEMPOTENCY_KEY_REUSED`.
- `request_hash = sha256(canonical_json(body))`, và hash phải ổn định khi đổi thứ tự key JSON.

SPEC chưa định nghĩa "canonical" cụ thể là gì, cũng chưa nói chuyện gì xảy ra khi nhiều request cùng key đến song song.

## Các phương án đã cân nhắc

**Dạng canonical của body:**
1. JSON Canonicalization (RFC 8785) trên body thô: giữ nguyên thứ tự mảng và các field lạ.
2. JSON của request **đã chuẩn hoá**: struct có thứ tự field cố định, `seat_ids` đã sắp xếp, field lạ bị từ chối từ trước.

**Request song song cùng key:**
1. Khoá theo key trước khi xử lý, bằng advisory lock của PostgreSQL hoặc `SET NX` trong Redis.
2. Suy `orderId` một cách tất định từ key, để các request trùng nhau giữ ghế bằng cùng một `orderId`.
3. Để ràng buộc `UNIQUE (user_id, idempotency_key)` phân xử, như luồng SPEC 9.1.

## Quyết định

- **Key** bắt buộc là UUID. Header thiếu hoặc sai định dạng thì trả `422 VALIDATION_ERROR`. Key được lưu ở dạng chuẩn (chữ thường, có gạch nối), nên các cách viết khác nhau của cùng một UUID là một key. Phạm vi của key là theo từng user.
- **Hash** dùng phương án 2: `sha256` trên JSON của request đã chuẩn hoá. Hai body khác nhau chỉ ở thứ tự key, khoảng trắng hoặc thứ tự ghế sẽ tạo ra đúng cùng một đơn, nên được coi là gửi lại chứ không phải xung đột. Field lạ bị từ chối (`400 BAD_REQUEST`), nên hash phủ toàn bộ những gì client gửi.
- **Chỉ lần tạo thành công mới được ghi nhận.** Request trả 409 hay 422 không để lại dấu vết gì, nên gửi lại với cùng key sẽ thực thi lại (và có thể thành công nếu ghế đã được nhả).
- **Gửi lại** trả `200` với trạng thái *hiện tại* của đơn, có thể đã là `CANCELLED`.
- **Song song** dùng phương án 3:
  - Ràng buộc unique là trọng tài.
  - Request thua cuộc nhận `200` nếu đơn thắng đã commit trước lúc nó tra cứu key.
  - Nếu nó đến bước giữ ghế trong lúc đơn thắng chưa commit, nó nhận `409 SEATS_UNAVAILABLE`, vì ghế đang bị chính đơn thắng giữ. **Hành vi này bị bác ở review M1**, xem mục cuối.
  - Khi đó client gửi lại cùng key sẽ nhận `200`.
  - Nhánh "vi phạm unique idempotency → quay lại bước 4" được cài đặt và thử lại đúng một lần.
- Không chọn phương án 1 vì nó thêm một round trip vào đường nóng của *mọi* request, chỉ để phục vụ một trường hợp hiếm. (Nhận định "hiếm" sau đó được chứng minh là sai; review M1 đã đổi quyết định này, xem mục cuối.)
- Không chọn phương án 2 vì SPEC yêu cầu `orderId` là UUIDv7. Hơn nữa, bước nhả ghế khi xung đột sẽ nhả luôn ghế của đơn thắng, do hai request dùng chung `orderId`.

## Hệ quả

- Test `TestCreateSameKeyInParallel` (10 request song song) luôn ra đúng 1 đơn. Một lần chạy ghi nhận: 1 tạo mới, 8 nhận lại đơn cũ, 1 nhận 409.
- Response của các request song song cùng key có thể khác nhau (`200` hoặc `409 SEATS_UNAVAILABLE`). Review M1 không chấp nhận điều này: đến M2, request trùng đang chạy sẽ nhận `409 IDEMPOTENCY_KEY_IN_PROGRESS` (xem mục cuối).

## Cập nhật sau review M1 (2026-09-25): request cùng key đang xử lý

### Yêu cầu

Request đến trong lúc một request cùng key đang xử lý **không được** trả `SEATS_UNAVAILABLE`: client sẽ hiểu nhầm là ghế đã bị người khác giữ, trong khi chính đơn của họ đang giữ. Phải trả `409 IDEMPOTENCY_KEY_IN_PROGRESS`. Sau khi request thắng hoàn tất, gửi lại cùng key phải nhận `200` với đúng đơn đó.

Đây không phải trường hợp hiếm. Ba lần chạy `TestCreateSameKeyInParallel` (10 request song song) cho ra 7, 1 và 0 request nhận `SEATS_UNAVAILABLE`.

### Các phương án để phát hiện "đang xử lý"

Cần một dấu hiệu dùng chung giữa các request, nghĩa là giữa các goroutine và cả giữa các instance booking, cho biết key đã có người nhận xử lý mà chưa có đơn trong PostgreSQL.

1. **Khoá Redis** `SET idem:<userId>:<key> <token> NX PX <ttl>`.
   - Mỗi request tốn thêm 2 lệnh Redis (lấy khoá, nhả khoá), không thêm tải cho PostgreSQL.
   - Process chết giữa chừng thì khoá tự hết hạn.
   - Đúng cả khi chạy nhiều instance.
2. **Advisory lock của PostgreSQL theo session** (`pg_try_advisory_lock`).
   - Khoá gắn với một connection, nên connection đó phải bị giữ suốt request.
   - Luồng tạo đơn lại cần thêm connection cho transaction giữ ghế và transaction ghi đơn, tức là 2 connection cho mỗi request.
   - Khi mọi connection trong pool đều đang giữ khoá và chờ thêm một connection nữa, hệ thống treo.
3. **Advisory lock theo transaction** (`pg_try_advisory_xact_lock`).
   - Khoá chỉ sống trong một transaction, trong khi luồng tạo đơn có hai transaction tách rời (giữ ghế, ghi đơn).
   - Muốn khoá phủ cả luồng thì phải gộp tất cả thành một transaction dài. Làm vậy vừa kéo dài thời gian giữ khoá hàng ghế, vừa không áp dụng được cho backend Redis.
4. **Bảng "claim" trong PostgreSQL** (INSERT khi nhận xử lý, DELETE khi xong).
   - Thêm 2 lần ghi PostgreSQL cho *mọi* request, kể cả các request sẽ nhận 409.
   - Đi ngược mục tiêu của backend Redis ở M2, vốn để request thất bại không phải ghi vào PostgreSQL.
   - Cần thêm bảng ngoài SPEC và cơ chế dọn claim bị bỏ dở.
5. **Map trong bộ nhớ process.** Chỉ đúng khi chạy một instance; hỏng ngay khi scale (HPA ở M9).

### Quyết định

Chọn phương án 1. Vì phương án hợp lý nhất cần Redis, phần này **làm ở M2** theo đúng chỉ đạo khi review. Thuật toán dự kiến:

1. Validate và tính `request_hash` như hiện nay.
2. `SET idem:<userId>:<key> <token ngẫu nhiên> NX PX 30000`. TTL phải dài hơn thời gian tối đa của một request (`WriteTimeout` của booking là 15 s).
3. Nếu **không lấy được khoá**, tra PostgreSQL theo `(user_id, idempotency_key)`:
   - đã có đơn và hash khớp: trả `200` với đơn đó;
   - đã có đơn nhưng hash khác: trả `422 IDEMPOTENCY_KEY_REUSED`;
   - chưa có đơn: trả `409 IDEMPOTENCY_KEY_IN_PROGRESS` kèm `Retry-After: 1`.
4. Nếu **lấy được khoá**, chạy luồng hiện tại (tra key, giữ ghế, ghi đơn). Luôn nhả khoá khi kết thúc, kể cả khi lỗi. Nhả khoá bằng script Lua kiểu compare-and-delete: chỉ xoá khi token khớp, để không xoá nhầm khoá của request khác sau khi khoá của mình đã hết hạn.
5. Ràng buộc `UNIQUE (user_id, idempotency_key)` vẫn là chốt chặn cuối. Nếu khoá hết hạn giữa chừng hoặc Redis mất dữ liệu, vẫn chỉ có tối đa một đơn, và nhánh "vi phạm unique → tra lại" được giữ nguyên.

Khoá này chỉ là trạng thái tạm. Mất khoá chỉ làm sai *mã phản hồi* cho các request trùng đang chạy song song, không bao giờ tạo ra đơn thứ hai. Vì vậy nó phù hợp với nguyên tắc "Redis dựng lại được / PostgreSQL là nguồn sự thật".

**Còn để ngỏ, sẽ chốt ở M2:** khi Redis không trả lời lúc lấy khoá, nên trả `503` (không nhận đơn khi không phân biệt được request trùng), hay bỏ qua khoá và dựa vào ràng buộc unique. Quyết định này sẽ đi cùng cách backend giữ ghế Redis xử lý khi Redis lỗi.

### Hiện trạng ở M1 và kiểm thử

- Hành vi `SEATS_UNAVAILABLE` cho request trùng đang chạy vẫn còn đến M2. Đây là hạn chế đã biết.
- Test đã có từ M1 và sẽ giữ nguyên ở M2:
  - `TestCreateSameKeyInParallel` (tầng service) và `TestSameIdempotencyKeyInParallelOverHTTP` (tầng HTTP): đúng 1 request nhận `201`; mọi `200` đều trỏ tới đúng đơn đó; sau khi request thắng hoàn tất, gửi lại cùng key 3 lần đều nhận `200` với đúng đơn đó.
- M2 sẽ siết điều kiện cho request thua: chỉ được nhận `200` (đúng đơn) hoặc `409 IDEMPOTENCY_KEY_IN_PROGRESS`, không còn `SEATS_UNAVAILABLE`.
