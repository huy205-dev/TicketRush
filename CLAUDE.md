# TicketRush

Hệ thống bán vé sự kiện chịu tải cao, viết bằng Go. Đây là dự án portfolio: mục tiêu là code đúng, có test, có số liệu đo thật, và giải thích được mọi quyết định.

**Đặc tả đầy đủ nằm trong `SPEC.md`. Đọc SPEC.md trước khi làm bất cứ việc gì.**

## Cách làm việc

- Làm theo milestone ở mục 12 của SPEC.md, **từng milestone một**, theo thứ tự M0 → M9.
- Đầu mỗi milestone: tóm tắt ngắn sẽ làm gì, liệt kê file sẽ tạo/sửa.
- Cuối mỗi milestone: chạy `make test` (và `make test-integration` nếu có), cập nhật tài liệu, báo lại những gì đã làm và tiêu chí hoàn thành nào đã đạt, rồi **dừng lại chờ người dùng review**. Không tự sang milestone tiếp theo.
- Nếu SPEC.md mâu thuẫn hoặc thiếu, hỏi lại thay vì tự đoán. Nếu buộc phải chọn, ghi lựa chọn vào ADR.
- Commit nhỏ, thường xuyên, message theo Conventional Commits (`feat(order): ...`, `test(inventory): ...`). Mỗi commit build và pass test.

## Quy tắc không được vi phạm

- **Không bao giờ bịa số liệu benchmark.** Chỉ ghi vào `docs/results.md` kết quả từ lần chạy thật, kèm cấu hình máy và commit hash. Nếu chưa chạy được thì ghi "chưa đo".
- PostgreSQL là nguồn sự thật. Redis chỉ giữ trạng thái tạm, phải dựng lại được từ PostgreSQL.
- Mọi thay đổi trạng thái đơn hàng: UPDATE có điều kiện `WHERE status = $expected`, và ghi outbox trong **cùng transaction**.
- Không gửi Kafka trực tiếp từ HTTP handler.
- Tiền là `int64` đơn vị đồng. Không dùng `float`.
- Mọi quyết định thời hạn dùng `now()` của PostgreSQL.
- Không commit secret. Mọi cấu hình qua biến môi trường, liệt kê trong `.env.example`.

## Quy ước code

- Go bản stable mới nhất (tối thiểu 1.23). Format bằng `gofmt`, lint bằng `go vet` và `staticcheck`.
- Truyền `context.Context` làm tham số đầu tiên cho mọi hàm có I/O.
- Wrap lỗi bằng `fmt.Errorf("...: %w", err)`. Lỗi nghiệp vụ là sentinel error trong package tương ứng (`order.ErrSeatsUnavailable`...), được map sang HTTP status ở `internal/httpx`.
- Log bằng `log/slog` dạng JSON, luôn kèm `request_id`, `order_id` khi có.
- Query SQL viết trong file `.sql`, sinh code bằng `sqlc`. Không nối chuỗi SQL.
- Không dùng biến global cho dependency; khởi tạo trong `main` và truyền vào.
- Test tích hợp đặt build tag `//go:build integration`, dùng testcontainers.
- Tên hàm, biến, comment trong code bằng tiếng Anh. Tài liệu trong `docs/` bằng tiếng Việt.

## Lệnh thường dùng

Tạo các target này trong `Makefile` ở M0 và giữ cho chúng luôn chạy được:

```
make up                # docker compose up -d (postgres, redis, redpanda...)
make down
make migrate           # goose up
make seed              # tạo sự kiện mẫu 5.000 ghế
make sqlc              # sinh code từ query
make run-booking       # và run-waitingroom, run-relay, run-expiry, run-ticket, run-refunder, run-notifier, run-fakepay
make test              # unit test
make test-integration  # test tích hợp
make lint
make bench             # seed → k6 → invariants → tóm tắt
make invariants
make chaos-redis       # và các kịch bản chaos khác
make k8s-up
```

## Môi trường

Người dùng phát triển trên MacBook (Apple Silicon), chạy container bằng OrbStack. Dùng image hỗ trợ `arm64`. Redpanda giới hạn `--smp 1 --memory 1G` để tiết kiệm RAM.

## Tài liệu phải cập nhật

- `docs/adr/`: mỗi quyết định kỹ thuật quan trọng một file.
- `docs/results.md`: mỗi lần đo.
- `docs/debug-journal.md`: mỗi lỗi khó gặp phải (triệu chứng, cách điều tra, nguyên nhân, cách sửa).
- `README.md`: cập nhật khi xong mỗi milestone.
