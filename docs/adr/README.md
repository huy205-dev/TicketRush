# Architecture Decision Records

Mỗi quyết định kỹ thuật quan trọng một file `NNN-ten-ngan.md`, viết theo mẫu:

```
# ADR-NNN: Tiêu đề

- Trạng thái: Đề xuất | Chấp nhận | Thay thế bởi ADR-XXX
- Ngày: YYYY-MM-DD

## Bối cảnh
## Các phương án đã cân nhắc
## Quyết định
## Hệ quả
## Số liệu (nếu có)
```

## Danh sách

| ADR | Chủ đề | Milestone | Trạng thái |
|---|---|---|---|
| [000](000-ban-goc-postgresql.md) | Làm bản gốc giữ ghế chỉ bằng PostgreSQL trước | M1 | Chấp nhận |
| 001 | Giữ ghế bằng Redis Lua | M2 | Chưa viết |
| 002 | Transactional outbox thay vì gửi Kafka trực tiếp | M3 | Chưa viết |
| 003 | Saga và bù trừ bằng hoàn tiền | M4 | Chưa viết |
| 004 | Cách chọn `ADMIT_BATCH` | M5 | Chưa viết |
| [005](005-idempotency-key.md) | Idempotency-Key cho API tạo đơn | M1 (xử lý request đang chạy: M2) | Chấp nhận |
| [006](006-lua-chon-khi-spec-chua-ro-m1.md) | Các lựa chọn ở M1 khi SPEC chưa nói rõ | M1 | Chấp nhận |

ADR 000–004 là các ADR SPEC.md yêu cầu. Từ 005 trở đi là các quyết định phát sinh trong lúc làm.
