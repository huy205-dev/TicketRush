# ADR-002: Transactional outbox thay vì gửi Kafka trực tiếp

- Trạng thái: Chấp nhận
- Ngày: 2026-09-25

## Bối cảnh

Mỗi lần đơn đổi trạng thái (giữ, huỷ, hết hạn; từ M4 còn có thanh toán, xuất vé, hoàn tiền), các service phía sau phải biết: ticket, notifier, refunder. Trạng thái nằm trong PostgreSQL, còn sự kiện đi qua Kafka. Ghi vào hai hệ thống không cùng một transaction là bài toán *dual write*:

- **Commit xong mới gửi:** process chết giữa hai bước, hoặc Kafka đang sập, thì sự kiện mất vĩnh viễn. Đơn đã `PAID` mà vé không bao giờ được xuất.
- **Gửi trước rồi mới commit:** transaction rollback thì consumer vẫn nhận một sự kiện chưa từng xảy ra, ví dụ xuất vé cho đơn không tồn tại.

CLAUDE.md cấm gửi Kafka trực tiếp từ HTTP handler, và yêu cầu mọi thay đổi trạng thái ghi outbox trong cùng transaction. ADR này giải thích vì sao, và cách relay được cài đặt.

## Các phương án đã cân nhắc

1. **Gửi Kafka trong handler, sau commit.** Mất sự kiện như mô tả ở trên. Ngoài ra request bị chặn chờ Kafka, và Kafka chậm hay sập kéo theo cả API bán vé.
2. **Transaction phân tán (2PC/XA).** Kafka không tham gia được XA, nên không khả thi.
3. **Outbox + relay đọc bằng polling** (SPEC 9.5). Sự kiện là một dòng trong bảng `outbox`, ghi cùng transaction với thay đổi trạng thái: commit thì cả hai cùng có, rollback thì cả hai cùng không. Một process riêng đọc outbox và gửi lên Kafka.
4. **Outbox + CDC** (Debezium đọc WAL). Không cần polling và độ trễ thấp hơn, nhưng thêm Kafka Connect, Debezium và cấu hình replication slot. Quá nặng cho quy mô dự án. Vì bảng outbox vẫn giữ nguyên, sau này đổi sang CDC không phải sửa phía ghi.
5. **Outbox + `LISTEN/NOTIFY` để đánh thức relay.** Giảm độ trễ khi hệ thống rảnh. SPEC coi đây là phần mở rộng; chưa làm.

## Quyết định

Chọn phương án 3.

- **Ghi:** `outbox.Write(ctx, tx, …)` là đường duy nhất để thêm sự kiện, và luôn nhận transaction đang thực hiện thay đổi trạng thái. Id của dòng outbox được lấy ngay trong câu `INSERT` (`nextval`), nên payload lưu sẵn `event_id` bằng chính id đó. `occurred_at` lấy theo đồng hồ PostgreSQL. Đang có `order.held` (tạo đơn), `order.cancelled` (huỷ) và `order.expired` (expiry worker, M3).
- **Relay** (`internal/outbox/relay.go`, `cmd/relay`):
  - Mỗi lô lấy tối đa 500 dòng chưa gửi theo thứ tự id, khoá bằng `FOR UPDATE SKIP LOCKED`.
  - Gửi bằng `ProduceSync` của franz-go (producer idempotent, `acks=all`). Key của record là `order_id`; header là `event_type` và `event_id` (id trong outbox).
  - Chỉ khi mọi record được Kafka xác nhận mới `UPDATE published_at`, trong cùng transaction với lúc khoá dòng.
  - Không còn gì để gửi thì chờ 100 ms; gặp lỗi thì chờ 1 s rồi thử lại.
  - Mỗi lô chạy tách khỏi tín hiệu dừng, nên SIGTERM không bao giờ cắt ngang giữa lúc gửi và lúc commit.
- **Topic:** relay tạo `orders.v1` và `tickets.v1` (6 partition, replication theo mặc định của broker) khi khởi động nếu chưa có, và thử lại cho đến khi Kafka trả lời.
- **Dọn dẹp:** cứ 10 phút, relay xoá các dòng đã gửi quá 7 ngày, mỗi lần tối đa 10.000 dòng.

## Hệ quả

- **Không mất sự kiện, không có sự kiện ma.** Sự kiện tồn tại khi và chỉ khi thay đổi trạng thái đã commit.
- **At-least-once, nên có thể trùng.** Nếu relay chết sau khi Kafka xác nhận nhưng trước khi commit, cả lô sẽ được gửi lại. Consumer (từ M4) phải khử trùng theo `event_id` và kiểm tra trạng thái đơn trước khi làm (SPEC 3.1).
- **Thứ tự theo đơn.**
  - Với một relay, sự kiện của cùng một đơn lên Kafka đúng thứ tự: cùng key thì cùng partition, và relay gửi theo thứ tự id.
  - Id tăng theo thứ tự commit với *cùng một đơn*, vì mọi thay đổi trạng thái của một đơn đều nối tiếp nhau nhờ khoá hàng và `UPDATE` có điều kiện.
  - Với nhiều relay chạy song song, hai lô khác nhau có thể được gửi xen nhau, nên hai sự kiện của cùng một đơn có thể đảo thứ tự. **Vận hành một relay cho mỗi cụm** là đủ: throughput đo được cao hơn nhu cầu nhiều bậc. Consumer vẫn phải kiểm tra trạng thái, không dựa vào thứ tự.
- **Độ trễ:** thêm tối đa khoảng 100 ms khi hệ thống rảnh (bước chờ khi không có gì để gửi), cộng thời gian một lô. Chưa đo riêng; metric `outbox_publish_lag_seconds` có ở M6.
- **Tải lên PostgreSQL:** mỗi relay một truy vấn mỗi 100 ms khi rảnh, rẻ nhờ index một phần `outbox_unpublished`. Thêm một dòng outbox cho mỗi lần đổi trạng thái.
- **Thêm một process phải vận hành.** Nếu relay dừng, sự kiện dồn lại trong outbox (không mất) và các service phía sau chậm theo. Metric `outbox_unpublished` (M6) dùng để cảnh báo.

## Số liệu

[docs/results.md, mục Outbox relay](../results.md#outbox-relay): relay xả một backlog 200.000 dòng lên Redpanda local với trung vị **101.085 dòng/s** (min 81.682, max 106.001; 3 lần). Tổng offset trên Kafka khớp chính xác số dòng đã gửi. Để so sánh: trong lúc mở bán, backend Redis tạo khoảng 2.400 đơn trong 12–16 giây. Relay không phải điểm nghẽn.

Test đầu–cuối `internal/e2e` chạy expiry worker và relay trong vòng lặp thật, với PostgreSQL, Redis và Redpanda thật: đơn giữ 2 giây chuyển `EXPIRED` sau 2,02–2,06 giây, và `order.held` rồi `order.expired` đến Kafka đúng thứ tự.
