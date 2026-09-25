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

Chưa có ADR nào. Các ADR SPEC.md yêu cầu:

| ADR | Chủ đề | Milestone |
|---|---|---|
| 000 | Lý do làm bản gốc chỉ dùng PostgreSQL | M1 |
| 001 | Giữ ghế bằng Redis Lua | M2 |
| 002 | Transactional outbox thay vì gửi Kafka trực tiếp | M3 |
| 003 | Saga và bù trừ bằng hoàn tiền | M4 |
| 004 | Cách chọn `ADMIT_BATCH` | M5 |
