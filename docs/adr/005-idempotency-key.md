# ADR-005: Idempotency-Key cho API tạo đơn

- Trạng thái: Chấp nhận
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
  - Nếu nó đến bước giữ ghế trong lúc đơn thắng chưa commit, nó nhận `409 SEATS_UNAVAILABLE`, vì ghế đang bị chính đơn thắng giữ.
  - Khi đó client gửi lại cùng key sẽ nhận `200`.
  - Nhánh "vi phạm unique idempotency → quay lại bước 4" được cài đặt và thử lại đúng một lần.
- Không chọn phương án 1 vì nó thêm một round trip vào đường nóng của *mọi* request, chỉ để phục vụ một trường hợp hiếm.
- Không chọn phương án 2 vì SPEC yêu cầu `orderId` là UUIDv7. Hơn nữa, bước nhả ghế khi xung đột sẽ nhả luôn ghế của đơn thắng, do hai request dùng chung `orderId`.

## Hệ quả

- Test `TestCreateSameKeyInParallel` (10 request song song) luôn ra đúng 1 đơn. Một lần chạy ghi nhận: 1 tạo mới, 8 nhận lại đơn cũ, 1 nhận 409.
- Response của các request song song cùng key có thể khác nhau (`200` hoặc `409`). Client nên gửi lại với cùng key khi gặp 409 trong lúc còn chờ response của chính mình.
- Nếu sau này cần mọi request song song cùng key đều nhận `200`, có thể thêm khoá theo key trong Redis ở M2. Việc đó chỉ tốn thêm chi phí cho nhánh 409.
