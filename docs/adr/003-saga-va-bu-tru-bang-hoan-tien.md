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
2. `INSERT payments … ON CONFLICT DO NOTHING`. Không có dòng trả về nghĩa là webhook trùng, không ghi gì thêm. Đây là lớp khử trùng theo `provider_txn_id`. Nếu đơn đã `PAID`/`TICKETED`, sau commit chạy lại `confirm` (idempotent): lần giao đầu có thể đã commit rồi mất phản hồi hoặc chết trước `confirm`, và lần gửi lại sẽ sửa Redis.
3. `FAILED`: ghi `IGNORED_FAILED`, không đổi đơn.
4. Đơn đã `PAID`/`TICKETED`/`REFUNDING`/`REFUNDED`: `DOUBLE_PAYMENT`, hoàn giao dịch này, **không đổi đơn** (xem mục "Thứ tự nhánh" bên dưới).
5. Sai số tiền: `AMOUNT_MISMATCH`, đơn sang `REFUNDING`, nhả ghế nếu đơn đang `HELD`.
6. `HELD` và còn hạn (theo `now()` của PostgreSQL): insert vé trong `SAVEPOINT`, rồi đơn sang `PAID`. Nếu vi phạm `UNIQUE (event_id, seat_id)`: rollback savepoint, log ERROR "double sell prevented", hoàn tiền với lý do `SEAT_CONFLICT`.
7. `HELD` đã quá hạn, hoặc `EXPIRED`: giữ lại ghế với cùng `orderId`, TTL 60 s, **ngay trong transaction**, lúc vẫn giữ khoá đơn. Giữ được và insert vé thành công thì `PAID`. Không giữ được thì hoàn tiền với lý do `LATE_PAYMENT_SEAT_TAKEN`. Giữ được nhưng insert vé vi phạm `UNIQUE` (kho ghế sai, giống bước 6) thì log ERROR "double sell prevented" và hoàn tiền với lý do `SEAT_CONFLICT`.
8. `CANCELLED`: `PAID_AFTER_CANCEL`, hoàn tiền.

Trừ việc giữ lại ghế ở bước 7 (quyết định phụ thuộc vào nó), kho ghế chỉ được động tới sau commit: `confirm` khi đơn thành `PAID`, `release` khi đơn `HELD` phải hoàn tiền. PostgreSQL là nguồn sự thật. Nếu Redis lỗi ở `confirm` hoặc `release` thì chỉ log cảnh báo, reconcile (M8) sẽ sửa. Nếu Redis lỗi ngay ở bước 7 thì cả transaction rollback và booking trả `500` để fakepay gửi lại, đúng SPEC 7.1 ("500 chỉ khi lỗi hạ tầng").

### Expiry worker nhả ghế dưới khoá đơn

Review M4 tìm ra một race giữa expiry worker và bước 7, và mình đã tái hiện được bằng test:
1. Expiry commit đơn `O` sang `EXPIRED`.
2. Webhook trễ của `O` (đang chờ khoá đơn) chạy tiếp và giữ lại ghế dưới đúng giá trị `held:O`.
3. Theo SPEC 9.4, expiry *sau commit* mới gọi `release.lua(O)`. Script này xoá luôn hold mới, vì không phân biệt được nó với hold cũ.
4. Người mua `B` giữ được ghế đó, trong khi PostgreSQL đã xuất vé cho `O`.
5. `confirm` của `O` gặp `held:B` nên dừng lại. Redis báo ghế đang giữ hoặc còn trống, dù ghế đã bán. Mỗi người mua sau đều trả tiền rồi bị hoàn tiền với lý do `SEAT_CONFLICT`, cho tới khi chạy reconcile.

Không ghế nào bị bán hai lần, vì `UNIQUE` vẫn chặn, nhưng người mua bị trừ tiền cho một ghế đã bán.

Cách sửa vẫn giữ đúng SPEC 9.4 (nhả ghế sau commit). Sau commit, expiry mở một transaction ngắn, khoá lại các đơn vừa hết hạn (`SELECT … FOR UPDATE`, theo thứ tự id), rồi gọi `release` ngay trong lúc giữ khoá. Đơn nào bước 7 đã chuyển sang `PAID`/`TICKETED` thì bỏ qua. Webhook trễ giữ khoá đơn từ lúc khoá tới lúc commit, nên hai bên giờ chạy tuần tự:
- webhook chạy trước: expiry thấy `PAID` và không nhả ghế;
- expiry chạy trước: webhook giữ lại ghế *sau khi* expiry đã nhả.

Test `TestExpiryReleaseDoesNotUndoLatePayment` ép đúng thứ tự trên bằng một wrapper tạm dừng `Release` và `Confirm`. Với code cũ, ở backend Redis, test fail: người mua khác giữ được ghế, ghế không thành `SOLD`. Với code mới thì pass. Backend PG không bị ở thứ tự này, vì vé đã commit trước `confirm`.

### Thứ tự nhánh: `DOUBLE_PAYMENT` đứng trước kiểm tra số tiền

SPEC 9.3 liệt kê kiểm tra số tiền (bước 4) trước nhánh trả hai lần (bước 8). Nếu làm đúng thứ tự đó, một giao dịch thứ hai **sai số tiền** cho đơn đã `PAID` sẽ đòi chuyển đơn sang `REFUNDING`. Nhưng máy trạng thái (SPEC 8) không có `PAID → REFUNDING`, và đổi trạng thái lúc này là sai: người mua đã trả đúng ở giao dịch đầu, vé vẫn hợp lệ. Hai phần của SPEC mâu thuẫn trong ca này. Mình chọn cách giữ đúng máy trạng thái và đúng ý của bước 8 ("không đổi trạng thái đơn"): đơn đã nhận tiền thì mọi giao dịch mới đều là `DOUBLE_PAYMENT`, bất kể số tiền. Với các đơn `HELD`, `EXPIRED`, `CANCELLED`, thứ tự vẫn đúng như SPEC.

Test: `TestDoublePayment` gửi giao dịch thứ hai với cả số tiền đúng lẫn số tiền sai, và kiểm tra đơn vẫn `PAID`.

### Refunder: `REFUNDING → REFUNDED` trừ trường hợp `DOUBLE_PAYMENT`

Hoàn tiền xong thì đơn `REFUNDING` sang `REFUNDED`. Với `DOUBLE_PAYMENT`, đơn chưa bao giờ rời `PAID`/`TICKETED`, nên refunder chỉ đánh dấu refund `SUCCEEDED` và phát `order.refunded`, không đụng tới đơn. Đơn đã `REFUNDING` vì lý do khác mà nhận thêm một giao dịch trùng thì cũng thế: chỉ refund của lý do gốc mới chuyển đơn sang `REFUNDED`.

### Idempotency ở từng bước

- **Webhook:** khoá chính `payments.provider_txn_id`. Test gửi cùng một webhook 3 lần song song: 1 `APPLIED`, 2 `DUPLICATE`, đúng một `order.paid`.
- **Ticket:** chỉ làm khi đơn `PAID`. `SetTicketQR` có điều kiện `qr_payload IS NULL`; đơn đã `TICKETED` thì bỏ qua. Test cho 4 lần giao `order.paid` song song: đúng một `ticket.issued`.
- **Refunder:** refund đã `SUCCEEDED` thì bỏ qua. `CompleteRefund` là `UPDATE … WHERE status = 'PENDING'`. API hoàn tiền của fakepay idempotent theo `provider_txn_id`, nên nếu chết giữa lúc gọi fakepay và lúc commit, lần chạy lại vẫn an toàn.
- **Notifier:** khoá Redis `notified:<event_type>:<order_id>`, thêm `:<provider_txn_id>` cho `order.refunded`, TTL 7 ngày.
  - Không dùng `event_id` của outbox: id đó đếm lại từ 1 sau khi reset hoặc restore DB, và sự kiện mới sẽ bị coi là đã gửi.
  - Khoá được đặt **sau** khi gửi xong. Chết giữa chừng thì gửi lại: thà gửi hai lần còn hơn không báo.
- **Consumer:** commit offset sau khi cả lô xử lý xong (`BlockRebalanceOnPoll`, tắt auto-commit), nên giao hàng là at-least-once.
  - Lệnh commit chạy tách khỏi tín hiệu dừng, nên dừng ngay sau một lô vẫn ghi nhận lô đó.
  - Lỗi tạm thời thì thử lại tại chỗ, backoff 100 ms–5 s.
  - Lỗi vĩnh viễn (không decode được, đơn hoặc refund không tồn tại) thì log ERROR và bỏ qua.

## Hệ quả

- **Nhất quán cuối cùng, không nhất quán tức thời.** Giữa lúc đơn `PAID` và lúc `TICKETED` có một khoảng trễ: relay (tới khoảng 100 ms khi rảnh) cộng thời gian của consumer. Chưa đo độ trễ này; khi chạy tay, đơn đã `TICKETED` ở lần poll đầu, khoảng 5 giây sau khi trả tiền. Client phải poll `GET /v1/orders/{id}`.
- **Webhook nào booking đã ghi nhận thì tiền không bị giữ mà không giao vé.** Mọi nhánh nhận tiền mà không giao vé đều ghi `refunds` và `order.refund_requested` trong cùng transaction với quyết định, và refund chỉ dừng khi fakepay xác nhận. Có hai trường hợp booking không ghi nhận gì, nên tiền sẽ mất nếu không đối soát với cổng thanh toán (chưa làm):
  - fakepay bỏ cuộc sau 8 lần gửi lại (khoảng 255 s) trong khi booking, PostgreSQL, hoặc Redis (ở bước 7) vẫn còn hỏng;
  - webhook cho một đơn không tồn tại: booking trả `200` và không ghi gì (SPEC 9.3 bước 1).
- **Một record lỗi mãi chặn cả consumer.** Ví dụ fakepay sập thì refunder thử lại mãi ở một refund. Consumer xử lý từng record một, nên mọi partition mà instance đó đang giữ đều phải chờ, không riêng partition của record lỗi. Việc rebalance của group cũng bị chặn. Quá rebalance timeout thì member bị loại khỏi group, và một member khác có thể nhận cùng lô đó song song. Handler idempotent nên hậu quả chỉ là làm lặp. Chấp nhận được với một cổng thanh toán duy nhất; nếu cần tách thì dùng topic retry hoặc DLQ (chưa làm).
- **Tiền về trễ với backend PG dùng hai kết nối cùng lúc.** Transaction webhook giữ một kết nối, còn `inventory.Hold` của backend PG lấy thêm một kết nối khác. Nếu rất nhiều webhook trễ đến cùng lúc, pool có thể cạn. Backend Redis không bị. Ghi lại để kiểm tra ở kịch bản `webhook_storm.js` (M7).
- **Hai lớp chống bán trùng được kiểm chứng:** `TestPaymentSeatConflict` và `TestLatePaymentSeatTicketedBehindInventory` giả lập kho ghế sai (vé đã thuộc đơn khác mà kho không biết). `UNIQUE` chặn lại, savepoint rollback cả những vé hợp lệ của đơn đó, và tiền được hoàn với lý do `SEAT_CONFLICT`.
- **Sau `SEAT_CONFLICT`, Redis vẫn sai.** Nhánh hoàn tiền nhả mọi ghế của đơn, đúng như SPEC, kể cả ghế mà PostgreSQL vừa cho thấy đã có vé. Ở backend Redis, ghế đó hiện là trống cho tới khi chạy reconcile (M8).
- **Chưa có metric** `double_sell_prevented_total` và `inventory_confirm_failures_total` (SPEC 9.3). Hiện chỉ có log, metric có ở M6.

### Các lựa chọn khác khi SPEC không nói rõ

- **Payload QR:** `base64url` không padding của chuỗi `ticketId + "." + hex(hmac_sha256(TICKET_SECRET, ticketId))`. SPEC 9.6 không nói MAC mã hoá thế nào; hex giữ cho payload chỉ có ký tự ASCII an toàn.
- **Webhook đúng chữ ký nhưng nội dung sai** (JSON hỏng, thiếu trường, trạng thái lạ, thanh toán `SUCCEEDED` với số tiền ≤ 0): trả `400`. SPEC 7.1 chỉ nói `200`/`401`/`500`. Không thể trả `200`, vì như vậy là nói đã ghi nhận trong khi thực ra không ghi gì.
- **fakepay chỉ gửi webhook về `PUBLIC_BASE_URL`** và không theo redirect. Đây là công cụ dev, nhưng không nên cho nó ký và gửi request tới bất kỳ địa chỉ nào người gọi đưa vào.

## Số liệu

Chưa đo hiệu năng saga (kịch bản `full_flow.js` và `webhook_storm.js` ở M7). Đã kiểm chứng tính đúng:

- test tích hợp cho mọi nhánh ở trên, chạy trên cả hai backend;
- test đầu–cuối qua Redpanda thật (`internal/e2e`);
- một lần mua bằng tay với 6 binary thật; xem `docs/progress/m4.md`.
