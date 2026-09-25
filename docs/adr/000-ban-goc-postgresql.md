# ADR-000: Làm bản gốc giữ ghế chỉ bằng PostgreSQL trước

- Trạng thái: Chấp nhận
- Ngày: 2026-09-25

## Bối cảnh

Yêu cầu cứng của TicketRush là không bao giờ bán trùng ghế. Mục tiêu hiệu năng là API giữ ghế đạt ≥ 5.000 req/s ở đỉnh với p99 < 200 ms. Kiến trúc cuối cùng giữ ghế bằng Redis Lua (M2), nhưng muốn nói Redis "nhanh hơn" thì phải có một con số để so. Muốn tin Redis "vẫn đúng" thì phải có một bản cài đặt mà tính đúng đắn hiển nhiên, dùng làm chuẩn đối chiếu.

## Các phương án đã cân nhắc

1. **Làm thẳng Redis.** Nhanh ra kết quả hơn, nhưng không có số liệu trước/sau. Khi có lỗi cũng không phân biệt được lỗi ở Redis hay ở luồng tạo đơn.
2. **PostgreSQL với khoá hàng bi quan:** `SELECT … FROM seats … ORDER BY seat_id FOR UPDATE`, sau đó kiểm tra `seat_holds` còn hạn và `tickets`, rồi upsert `seat_holds`. Tính đúng đắn đến từ row lock, là thứ dễ lập luận và dễ kiểm chứng.
3. **PostgreSQL kiểu lạc quan:** `INSERT … ON CONFLICT` thẳng vào `seat_holds`, không khoá `seats`. Ít round trip hơn, nhưng xử lý hold đã hết hạn và "tất cả hoặc không ghế nào" với nhiều ghế trở nên phức tạp.
4. **Transaction `SERIALIZABLE`.** Đúng nhưng phải thử lại khi gặp lỗi serialization. Dưới tranh chấp cao, số lần thử lại tăng vọt và latency khó đoán.

## Quyết định

Chọn phương án 2, đúng như SPEC mục 9.2, và đặt sau interface `inventory.Inventory`. Mọi thứ khác được giữ giống hệt cho cả hai backend, để phép so sánh ở M2 chỉ đo đúng một biến:

- Luồng tạo đơn (`order.Service.Create`) không phụ thuộc backend. Nó gồm hai transaction: giữ ghế, rồi ghi đơn + `order_seats` + outbox. Nếu bước ghi đơn lỗi thì nhả ghế. Luồng này đúng như SPEC 9.1.
- Khoá theo thứ tự `seat_id` để hai request giữ các nhóm ghế chồng nhau không deadlock.
- Kiểm tra `seat_holds` bằng một câu lệnh mới *sau khi* đã lấy khoá. Ở `READ COMMITTED`, mỗi câu lệnh nhận snapshot riêng, nên câu này thấy được hold do transaction giữ khoá trước đã commit.
- Sơ đồ ghế được render một lần mỗi 500 ms và nén gzip, dùng chung cho cả hai backend. Nếu chỉ bản Redis có cache, phần tăng tốc do cache sẽ bị tính nhầm cho Redis.
- Việc kiểm tra giờ mở bán hỏi `now()` của PostgreSQL. Khi đã mở bán thì kết quả được cache (thời gian không chạy lùi), để không tốn một round trip cho mỗi request.

## Hệ quả

- Tính đúng đắn được chứng minh bằng test tích hợp trên PostgreSQL thật. 1.000 goroutine cùng giữ `VIP-A-1` thì đúng 1 thành công, cả ở tầng `Inventory` lẫn qua toàn bộ luồng tạo đơn. 200 goroutine giữ ngẫu nhiên 2 ghế trong 20 ghế thì không ghế nào thuộc hai đơn.
- Đổi backend chỉ bằng biến `INVENTORY_BACKEND`, và bản PG vẫn là phương án dự phòng.
- Mỗi lần giữ ghế tốn một transaction với 3 câu lệnh. Các request tranh cùng ghế VIP phải xếp hàng chờ khoá, và đó là cái giá mà bản Redis cần xoá bỏ.
- Hai transaction tách rời: nếu process chết giữa bước giữ ghế và bước ghi đơn, dòng `seat_holds` còn lại đến hết `HOLD_TTL + HOLD_GRACE` rồi tự hết hạn. Không bán trùng, chỉ tạm khoá ghế.
- Backend PG không có bộ đếm version cho sơ đồ ghế, nên `version` luôn bằng 0 (bản Redis ở M2 sẽ có).

## Số liệu

Xem [docs/results.md, mục Baseline PG](../results.md#baseline-pg). Tóm tắt, đo trên laptop với k6 chạy cùng máy:
- Chế độ `BUYER_MODE=vu`: p99 giữ ghế 35–175 ms, 0% lỗi.
- Chế độ `iteration`: bán hết 5.000 ghế trong khoảng 11 giây, đỉnh 2.209 req/s, p99 839 ms, 0% lỗi. Không đạt ngưỡng p99 < 200 ms.
