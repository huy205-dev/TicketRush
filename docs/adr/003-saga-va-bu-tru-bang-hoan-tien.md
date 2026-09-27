# ADR-003: Saga và bù trừ bằng hoàn tiền

- Trạng thái: Chấp nhận
- Ngày: 2026-09-27

## Bối cảnh

Mua một vé đi qua nhiều hệ thống không chung transaction:

1. booking giữ ghế (Redis hoặc PostgreSQL);
2. người mua trả tiền ở cổng thanh toán (fakepay, đóng vai hệ thống bên ngoài);
3. webhook báo kết quả về booking;
4. ticket service sinh QR;
5. notifier báo cho người mua.

Cổng thanh toán không nằm trong transaction của mình, và tiền đã trừ thì không "rollback" được. fakepay còn cố tình gây lỗi (SPEC 7.3):

- webhook đến trễ, có khi trễ 11 phút, tức là sau khi hết giữ ghế;
- webhook đến 2–4 lần;
- webhook bị gửi lại dù booking đã nhận;
- người mua huỷ đơn rồi vẫn trả tiền, hoặc trả hai lần.

Cần một cách để mọi nhánh đều kết thúc ở trạng thái nhất quán: hoặc người mua có vé, hoặc được trả lại tiền. Không nhánh nào được giữ tiền mà không giao vé, và không ghế nào được bán hai lần.

## Các phương án đã cân nhắc

1. **Transaction phân tán (2PC) giữa booking và cổng thanh toán.** Cổng thanh toán thật không tham gia 2PC. Không khả thi.
2. **Chặn thanh toán khi đơn không còn hợp lệ** (ví dụ: không cho mở trang thanh toán khi đơn đã hết hạn). Không đủ: người mua có thể bấm trả tiền 1 giây trước khi hết hạn, và webhook đến 11 phút sau. Trạng thái của mình và của cổng thanh toán luôn có thể lệch nhau.
3. **Saga có bù trừ.** Mỗi bước là một transaction cục bộ, nối nhau qua sự kiện (outbox + Kafka, ADR-002). Khi tiền đã về mà không giao được vé, bước bù trừ là **hoàn tiền**. Đây là cách SPEC 9.3 và 9.7 mô tả.
4. **Giữ ghế cho đến khi webhook về, không có hạn.** Tránh được mọi ca tiền về trễ, nhưng người mua bỏ ngang thì ghế bị khoá mãi. Trái với mục tiêu bán hết vé nhanh.

## Quyết định

Chọn phương án 3: **saga dạng choreography**. Mỗi service phản ứng với sự kiện, không có bộ điều phối trung tâm.

| Bước | Ai làm | Transaction cục bộ | Sự kiện phát ra |
|---|---|---|---|
| Giữ ghế | booking (`POST /v1/orders`) | đơn `HELD` + ghế | `order.held` |
| Nhận tiền | booking (`POST /v1/webhooks/fakepay`) | `payments`, vé, đơn `PAID` **hoặc** `refunds`, đơn `REFUNDING` | `order.paid` **hoặc** `order.refund_requested` |
| Xuất vé | ticket (`order.paid`) | QR cho từng vé, đơn `TICKETED` | `ticket.issued` |
| Bù trừ | refunder (`order.refund_requested`) | gọi fakepay, `refunds.status = SUCCEEDED`, đơn `REFUNDED` | `order.refunded` |
| Thông báo | notifier (`ticket.issued`, `order.refunded`) | không có (chỉ ghi dấu đã gửi) | — |

**Webhook** (`order.Service.ApplyPayment`) là nơi ra quyết định. Mọi nhánh chạy trong **một** transaction, với dòng `orders` bị khoá `FOR UPDATE`:

1. Không có đơn: bỏ qua, trả `200` để fakepay không gửi lại mãi.
2. `INSERT payments … ON CONFLICT DO NOTHING`. Không có dòng trả về nghĩa là webhook trùng, không làm gì thêm. Đây là lớp khử trùng theo `provider_txn_id`.
3. `FAILED`: ghi `IGNORED_FAILED`, không đổi đơn.
4. Đơn đã `PAID`/`TICKETED`/`REFUNDING`/`REFUNDED`: `DOUBLE_PAYMENT`, hoàn giao dịch này, **không đổi đơn** (xem mục "Thứ tự nhánh" bên dưới).
5. Sai số tiền: `AMOUNT_MISMATCH`, đơn sang `REFUNDING`, nhả ghế nếu đơn đang `HELD`.
6. `HELD` và còn hạn (theo `now()` của PostgreSQL): insert vé trong `SAVEPOINT`, rồi đơn sang `PAID`. Nếu vi phạm `UNIQUE (event_id, seat_id)`: rollback savepoint, log ERROR "double sell prevented", hoàn tiền với lý do `SEAT_CONFLICT`.
7. `HELD` đã quá hạn, hoặc `EXPIRED`: giữ lại ghế với cùng `orderId`, TTL 60 s. Giữ được và insert vé thành công thì `PAID`; ngược lại hoàn tiền với lý do `LATE_PAYMENT_SEAT_TAKEN`.
8. `CANCELLED`: `PAID_AFTER_CANCEL`, hoàn tiền.

Sau commit mới động vào kho ghế: `confirm` khi đơn thành `PAID`, `release` khi đơn `HELD` phải hoàn tiền. PostgreSQL là nguồn sự thật. Nếu Redis lỗi ở bước này thì chỉ log cảnh báo, reconcile (M8) sẽ sửa.

### Thứ tự nhánh: `DOUBLE_PAYMENT` đứng trước kiểm tra số tiền

SPEC 9.3 liệt kê kiểm tra số tiền (bước 4) trước nhánh trả hai lần (bước 8). Nếu làm đúng thứ tự đó, một giao dịch thứ hai **sai số tiền** cho đơn đã `PAID` sẽ đòi chuyển đơn sang `REFUNDING`. Nhưng máy trạng thái (SPEC 8) không có `PAID → REFUNDING`, và đổi trạng thái lúc này là sai: người mua đã trả đúng ở giao dịch đầu, vé vẫn hợp lệ. Hai phần của SPEC mâu thuẫn trong ca này. Mình chọn cách giữ đúng máy trạng thái và đúng ý của bước 8 ("không đổi trạng thái đơn"): đơn đã nhận tiền thì mọi giao dịch mới đều là `DOUBLE_PAYMENT`, bất kể số tiền. Với các đơn `HELD`, `EXPIRED`, `CANCELLED`, thứ tự vẫn đúng như SPEC.

Test: `TestDoublePayment` gửi giao dịch thứ hai với cả số tiền đúng lẫn số tiền sai, và kiểm tra đơn vẫn `PAID`.

### Refunder: `REFUNDING → REFUNDED` trừ trường hợp `DOUBLE_PAYMENT`

Hoàn tiền xong thì đơn `REFUNDING` sang `REFUNDED`. Với `DOUBLE_PAYMENT`, đơn chưa bao giờ rời `PAID`/`TICKETED`, nên refunder chỉ đánh dấu refund `SUCCEEDED` và phát `order.refunded`, không đụng tới đơn. Đơn đã `REFUNDING` vì lý do khác mà nhận thêm một giao dịch trùng thì cũng thế: chỉ refund của lý do gốc mới chuyển đơn sang `REFUNDED`.

### Idempotency ở từng bước

- **Webhook:** khoá chính `payments.provider_txn_id`. Test gửi cùng một webhook 3 lần song song: 1 `APPLIED`, 2 `DUPLICATE`, đúng một `order.paid`.
- **Ticket:** chỉ làm khi đơn `PAID`. `SetTicketQR` có điều kiện `qr_payload IS NULL`; đơn đã `TICKETED` thì bỏ qua. Test cho 4 lần giao `order.paid` song song: đúng một `ticket.issued`.
- **Refunder:** refund đã `SUCCEEDED` thì bỏ qua. `CompleteRefund` là `UPDATE … WHERE status = 'PENDING'`. API hoàn tiền của fakepay idempotent theo `provider_txn_id`, nên nếu chết giữa lúc gọi fakepay và lúc commit, lần chạy lại vẫn an toàn.
- **Notifier:** khoá Redis `notified:<event_id>` (TTL 7 ngày). Khoá được đặt **sau** khi gửi xong: chết giữa chừng thì gửi lại, thà gửi hai lần còn hơn không báo.
- **Consumer:** commit offset sau khi cả lô xử lý xong (`BlockRebalanceOnPoll`, tắt auto-commit), nên giao hàng là at-least-once. Lỗi tạm thời thì thử lại tại chỗ với backoff 100 ms–5 s. Lỗi vĩnh viễn (không decode được, đơn hoặc refund không tồn tại) thì log ERROR và bỏ qua.

## Hệ quả

- **Nhất quán cuối cùng, không nhất quán tức thời.** Giữa lúc đơn `PAID` và lúc `TICKETED` có một khoảng trễ (relay khoảng 100 ms + consumer). Trong lúc chạy tay trên máy local là dưới 1 giây. Client phải poll `GET /v1/orders/{id}`.
- **Không mất tiền của người mua.** Mọi nhánh nhận tiền mà không giao vé đều ghi `refunds` và `order.refund_requested` trong cùng transaction với quyết định. Refund chỉ dừng khi fakepay xác nhận.
- **Một refund treo chặn partition.** fakepay sập thì refunder thử lại mãi ở record đó, và các sự kiện sau nó trên cùng partition phải chờ. Chấp nhận được với một cổng thanh toán duy nhất. Nếu cần tách thì dùng topic retry hoặc DLQ (chưa làm).
- **Tiền về trễ với backend PG dùng hai kết nối cùng lúc.** Transaction webhook giữ một kết nối, còn `inventory.Hold` của backend PG lấy thêm một kết nối khác. Nếu rất nhiều webhook trễ đến cùng lúc, pool có thể cạn. Backend Redis không bị. Ghi lại để kiểm tra ở kịch bản `webhook_storm.js` (M7).
- **Hai lớp chống bán trùng được kiểm chứng:** `TestPaymentSeatConflict` giả lập kho ghế sai (vé đã thuộc đơn khác mà kho không biết). `UNIQUE` chặn lại, savepoint rollback cả những vé hợp lệ của đơn đó, và tiền được hoàn với lý do `SEAT_CONFLICT`.
- **Chưa có metric** `double_sell_prevented_total` và `inventory_confirm_failures_total` (SPEC 9.3). Hiện chỉ có log, metric có ở M6.

## Số liệu

Chưa đo hiệu năng saga (kịch bản `full_flow.js` và `webhook_storm.js` ở M7). Đã kiểm chứng tính đúng:

- test tích hợp cho mọi nhánh ở trên, chạy trên cả hai backend;
- test đầu–cuối qua Redpanda thật (`internal/e2e`);
- một lần mua bằng tay với 6 binary thật; xem `docs/progress/m4.md`.
