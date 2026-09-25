# ADR-001: Giữ ghế bằng Redis Lua

- Trạng thái: Chấp nhận
- Ngày: 2026-09-25

## Bối cảnh

Bản gốc PostgreSQL ([ADR-000](000-ban-goc-postgresql.md)) đúng, nhưng mỗi lần giữ ghế tốn một transaction gồm 3 câu lệnh. Các request tranh cùng một ghế phải xếp hàng chờ khoá hàng, và cả các request *thất bại* (409, chiếm khoảng 5/6 số request khi mở bán) cũng chiếm một kết nối trong pool nhỏ của PostgreSQL. Mục tiêu là giữ ghế nhanh, và để request thất bại không phải khoá gì trong PostgreSQL. Tính đúng đắn vẫn giữ nguyên: không bao giờ bán trùng.

## Các phương án đã cân nhắc

1. **Script Lua cho cả nhóm ghế** (SPEC 6.3–6.5). Redis chạy script một cách nguyên tử, nên "kiểm tra mọi ghế rồi ghi mọi ghế" không cần khoá, và chỉ tốn một round trip.
2. **`SET NX` từng ghế từ phía client.** Không nguyên tử cho nhiều ghế: giữ được một phần thì phải tự nhả, và trong lúc đó người khác thấy ghế bị giữ oan.
3. **`WATCH`/`MULTI`/`EXEC`.** Là optimistic locking: dưới tranh chấp cao, `EXEC` thất bại liên tục và client phải thử lại. Cách này tệ nhất đúng vào lúc mở bán.
4. **Redlock hoặc khoá phân tán theo ghế.** Phức tạp, tốn nhiều round trip, và không cần thiết với một Redis chính.

## Quyết định

- Chọn phương án 1, dùng đúng 3 script của SPEC (`internal/inventory/lua/`). Script được nhúng bằng `go:embed` và chạy qua `redis.Script` (dùng `EVALSHA`, tự fallback sang `EVAL`). Backend Redis cài đặt cùng interface `inventory.Inventory` với backend PG; chọn backend bằng `INVENTORY_BACKEND` (mặc định `redis`).
- Mỗi ghế là một key `seat:{<eventId>}:<seatId>`. Không có key nghĩa là ghế trống, nên không cần seed Redis. Giá trị là `held:<orderId>` (có TTL) hoặc `sold:<orderId>` (không TTL). `{eventId}` là hash tag: mọi key của một sự kiện nằm cùng slot khi chạy Redis Cluster, nên script nhiều key không bị lỗi `CROSSSLOT`.
- TTL trong Redis bằng `HOLD_TTL + HOLD_GRACE`, trong khi PostgreSQL chỉ nhận thanh toán đến `hold_expires_at = now() + HOLD_TTL`. Nhờ 30 giây dư, không ai giữ lại được ghế trước khi PostgreSQL chắc chắn đã coi đơn cũ là hết hạn.
- `Status` đọc trạng thái bằng `MGET` theo lô 500 key, gửi trong một pipeline. Sơ đồ ghế vẫn được cache 500 ms như ở M1.
- Mỗi lần ghế đổi trạng thái thành công (hold, release có nhả ghế, confirm), backend tăng `seatmap:{eid}:version` bằng một lệnh `INCR` riêng, đặt ngay trong backend. Lý do:
  - SPEC chỉ nêu bước này trong luồng tạo đơn (9.1 bước 10), nhưng định nghĩa key là "tăng mỗi lần ghế đổi trạng thái";
  - đặt trong backend thì nhả ghế khi huỷ đơn và khi hết hạn (M3) cũng được tính.

  Không gộp `INCR` vào script, để giữ nguyên văn script của SPEC. Version chỉ là gợi ý cho client, nên `INCR` lỗi thì chỉ ghi log cảnh báo, không làm hỏng thao tác đã thành công.

## Hệ quả

- Request giữ ghế thất bại không còn chạm khoá nào trong PostgreSQL. Nó vẫn chạm PostgreSQL một lần để tra Idempotency-Key (bước 4 của SPEC).
- Trạng thái ghế giờ nằm ở hai nơi. Redis chỉ là trạng thái tạm và phải dựng lại được từ PostgreSQL. Reconcile sau khi Redis mất dữ liệu làm ở M8. Trước khi có M8, nếu Redis mất dữ liệu thì ghế đang giữ bị "nhả" trong Redis dù đơn vẫn `HELD`; lớp chặn cuối là ràng buộc `UNIQUE (event_id, seat_id)` của `tickets` lúc thanh toán (M4).
- `confirm.lua` có thể đã ghi `sold` cho một phần ghế trước khi gặp ghế sai chủ, vì script dừng ngay ở ghế đó. Tình huống này không thể xảy ra trong hệ thống đúng; reconcile (M8) sẽ sửa.
- Test "hợp đồng" chạy cho cả hai backend trên Redis và PostgreSQL thật, gồm 1.000 goroutine cùng giữ một ghế và 200 goroutine giữ ngẫu nhiên các cặp trong 20 ghế. Ngoài ra có test riêng cho từng script.
- Sau mỗi lần đo tải, `loadtest/check_redis.sh` so khớp toàn bộ key trong Redis với PostgreSQL.

## Số liệu

[docs/results.md, mục So sánh PG và Redis](../results.md#so-sánh-pg-và-redis). Đo 3 lần mỗi backend trên cùng commit và cùng máy (laptop, k6 chạy cùng máy), trung vị Redis so với PG:
- RPS giữ ghế đỉnh: 2.901 so với 2.070 req/s (+40%);
- p50: 48 so với 462 ms (−90%); p95: 502 so với 974 ms (−48%);
- p99: 958 so với 1.062 ms (−10%, khoảng min–max của hai bên chồng nhau).

Cả hai backend đều 0% lỗi, không ghế nào bán trùng, và Redis khớp PostgreSQL 5.000/5.000 ghế. Đuôi p99 chưa cải thiện; giả thuyết (chưa kiểm chứng) là do pool PostgreSQL vẫn nằm trên đường đi của mọi request tạo đơn. Việc này để M7 điều tra.
