# Nhật ký gỡ lỗi

Mỗi lỗi khó gặp phải được ghi theo mẫu dưới đây, mới nhất ở trên cùng.

```
## YYYY-MM-DD — Tiêu đề ngắn

**Triệu chứng:** quan sát được gì, ở đâu.
**Cách điều tra:** đã thử gì, dùng công cụ nào, loại trừ giả thuyết nào.
**Nguyên nhân gốc:**
**Cách sửa:** commit nào.
**Bài học:**
```

## 2026-09-25 — So khớp Redis với PostgreSQL báo 10.000 vi phạm dù dữ liệu đúng

**Triệu chứng:** lần chạy thử đầu tiên của `loadtest/check_redis.sh` (so sánh key `seat:{1}:*` trong Redis với các đơn `HELD` trong PostgreSQL) báo `redis_key_not_in_pg=5000` và `pg_state_missing_in_redis=5000`. Nghĩa là không dòng nào khớp, trong khi các kiểm tra SQL khác và toàn bộ test tích hợp đều đạt.

**Cách điều tra:**
1. Cả hai phía đều đúng 5.000 dòng. Lệch toàn bộ chứ không phải một phần, nên nghi ngờ định dạng trước dữ liệu.
2. In byte thô của hai phía bằng `od -c`:
   - Redis trả `seat:{1}:VIP-I-29 held:01a0…`, đúng như mong đợi;
   - PostgreSQL trả `VIP-A-29 held 01a0…`.
   Phía Redis vẫn còn nguyên tiền tố `seat:{1}:`, tức bước `awk` không cắt được nó.
3. Bước cắt dùng `sub(prefix, "", $1)`. Tham số đầu của `sub` là một biểu thức chính quy, không phải chuỗi.

**Nguyên nhân gốc:** trong regex, `{1}` là lượng từ "lặp đúng 1 lần" của ký tự đứng trước. Vì vậy `seat:{1}:` được hiểu là `seat` + `:` (một lần) + `:`, tức chuỗi `seat::`, và không bao giờ khớp với tên key có hash tag.

**Cách sửa:** commit `0cc0197`. So tiền tố như một chuỗi thường: `index($1, prefix) == 1`, rồi cắt bằng `substr`. Mình kiểm chứng lại bằng cách cố ý thêm một key giả vào Redis: script báo đúng 1 vi phạm, và báo 0 sau khi xoá key đó.

**Bài học:** hash tag `{…}` của Redis Cluster trùng cú pháp lượng từ của regex. Mọi chỗ xử lý tên key bằng công cụ dùng regex (`awk`, `sed`, `grep`) phải escape dấu ngoặc nhọn hoặc so sánh như chuỗi thường. Và một script kiểm tra phải được thử với dữ liệu sai (negative test) trước khi tin vào kết quả "0 vi phạm" của nó.

## 2026-09-25 — sqlc báo "2 edits overlap" và "syntax error at or near …" với query hoàn toàn hợp lệ

**Triệu chứng:** `sqlc generate` (v1.31.1) lỗi trên nhiều file cùng lúc:
- `internal/catalog/queries.sql:20:1: 2 edits overlap`
- `internal/inventory/queries.sql:6:1: edited query syntax is invalid: syntax error at or near "st"`
- `column reference "event_id" is ambiguous`

Mọi query chạy tốt khi dán thẳng vào `psql`. Chữ "st" và "NG" trong thông báo lỗi không có nghĩa gì trong query, gợi ý rằng sqlc đang cắt chuỗi sai vị trí.

**Cách điều tra:**
1. Thông báo "edited query" cho thấy lỗi nằm ở bước sqlc thay tham số có tên (`@event_id`) bằng `$1, $2…`. Bước này sửa chuỗi SQL theo offset.
2. Các query lỗi đều có comment đứng *trước* dòng `-- name:`. Chuyển comment xuống *sau* dòng `-- name:` (cũng là chỗ sqlc lấy làm doc comment cho Go) thì lỗi "syntax error near st" biến mất.
3. Lỗi "2 edits overlap" vẫn còn. Mình tách từng biến thể của `CreateEvent` ra một thư mục thử nghiệm riêng để thu hẹp phạm vi:
   - tham số có tên, viết trên một dòng: OK;
   - tham số có tên, viết nhiều dòng: OK;
   - thêm `now() + (@x::bigint * interval '1 millisecond')`: **lỗi**;
   - đổi sang `make_interval(secs => @x::bigint)`: OK.

**Nguyên nhân gốc:** hai lỗi của sqlc khi thay tham số có tên. (1) Comment trước `-- name:` làm lệch offset của các vị trí cần thay. (2) Literal có kiểu (`interval '1 millisecond'`) đứng cạnh tham số có tên khiến hai lần thay đè lên nhau. Lỗi "ambiguous" là hệ quả của câu lệnh đã bị cắt sai.

**Cách sửa:** commit `e969e7b`.
- Luôn đặt comment của query sau dòng `-- name:`.
- Truyền thời lượng dưới dạng một tham số `@ttl::interval`, và override kiểu `pg_catalog.interval` sang `time.Duration` trong `sqlc.yaml` (pgx v5 tự encode `time.Duration` thành interval). Làm vậy vừa tránh được lỗi, vừa gọn hơn cách nhân với literal.
- `unnest` hai mảng trong mệnh đề `FROM` cũng không được sqlc hiểu (`function unnest(unknown, unknown) does not exist`). Mình chuyển sang dùng hai `unnest` trong danh sách `SELECT`; PostgreSQL ≥ 10 ghép chúng theo từng cặp.

**Bài học:** khi công cụ sinh code báo lỗi cú pháp trên một query chạy đúng trong `psql`, hãy nghi ngờ bước biến đổi của công cụ trước, rồi thu hẹp bằng các ví dụ tối thiểu tách riêng. `make lint` giờ chạy `sqlc diff`, nên code sinh ra luôn khớp với query.
