# TicketRush

Hệ thống bán vé sự kiện chịu tải cao, viết bằng Go.

Khi mở bán một concert, hàng chục nghìn người cùng bấm vào vài trăm ghế VIP trong vài giây. Hệ thống phải: không bao giờ bán một ghế cho hai người, giữ ghế có thời hạn trong lúc người mua thanh toán, xếp hàng người mua qua phòng chờ ảo, xử lý đúng webhook thanh toán bị trùng hoặc đến trễ, và vẫn đúng khi Redis, Kafka hay chính service chết giữa chừng.

Đặc tả đầy đủ: [SPEC.md](SPEC.md).

> **Trạng thái:** xong **M2 – giữ ghế bằng Redis Lua**. Hệ thống đã có:
> - giữ ghế bằng Redis (mặc định) hoặc PostgreSQL;
> - tạo, xem, huỷ đơn với Idempotency-Key, dùng khoá Redis cho các request trùng đang chạy;
> - số liệu so sánh PG và Redis.
>
> Chưa có: expiry worker, relay Kafka, thanh toán, phòng chờ.

## Demo

Chưa có (dự kiến sau M5).

## Kiến trúc

```mermaid
flowchart LR
  U[Người mua] --> G[Nginx<br/>rate limit]
  G --> W[waitingroom]
  G --> B[booking]
  W -- admission token --> U
  B -- Lua giữ ghế --> R[(Redis)]
  W --> R
  B -- đơn + outbox --> P[(PostgreSQL)]
  REL[relay] -- đọc outbox --> P
  REL --> K{{Redpanda}}
  K --> T[ticket]
  K --> N[notifier]
  K --> RF[refunder]
  FP[fakepay] -- webhook --> B
  RF -- refund API --> FP
  EX[expiry] --> P
  EX --> R
```

PostgreSQL là nguồn sự thật; Redis chỉ giữ trạng thái tạm và dựng lại được từ PostgreSQL. Chi tiết ở [SPEC.md mục 3](SPEC.md#3-kiến-trúc-tổng-thể).

## Kết quả đo

Đo bằng `make bench-hold BACKEND=<pg|redis>`:
- 3 lần mỗi backend, mỗi lần reset DB và seed lại;
- kịch bản mở bán 2.000 VU, mỗi vòng lặp là một người mua mới;
- cả hai backend chạy cùng commit (`0cc0197`), trên MacBook Apple M5 Pro 24 GiB, k6 chạy cùng máy, nên chỉ để so sánh tương đối.

Chi tiết và nhận xét ở [docs/results.md](docs/results.md#so-sánh-pg-và-redis).

| Backend (trung vị [min–max]) | RPS giữ ghế đỉnh | p50 | p95 | p99 | Lỗi | Ghế bán trùng |
|---|---|---|---|---|---|---|
| pg | 2.070 [1.762–2.154] | 462 ms [446–489] | 974 ms [728–1.035] | 1.062 ms [959–1.115] | 0% | 0 |
| redis | **2.901** [2.775–2.971] | **48 ms** [44–63] | **502 ms** [454–542] | 958 ms [765–1.100] | 0% | 0 |

Redis: đỉnh +40%, p50 −90%, p95 −48%, nhưng p99 chỉ −10%. Vẫn chưa đạt mục tiêu ≥ 5.000 req/s và p99 < 200 ms.

## Quyết định kỹ thuật

Danh sách đầy đủ ở [docs/adr/](docs/adr/README.md).

- [ADR-000](docs/adr/000-ban-goc-postgresql.md): làm bản gốc giữ ghế chỉ bằng PostgreSQL (`SELECT … FOR UPDATE`) trước khi dùng Redis.
- [ADR-001](docs/adr/001-giu-ghe-bang-redis-lua.md): giữ ghế bằng script Lua trong Redis; hash tag theo sự kiện; `HOLD_GRACE`.
- [ADR-005](docs/adr/005-idempotency-key.md): Idempotency-Key: hash trên request đã chuẩn hoá. Khoá Redis cho request đang chạy trả `409 IDEMPOTENCY_KEY_IN_PROGRESS`; khi Redis không trả lời thì trả `503` (fail closed).
- [ADR-006](docs/adr/006-lua-chon-khi-spec-chua-ro-m1.md): các lựa chọn khi SPEC chưa nói rõ ở M1.

## API (booking, `:8080`)

| Method | Đường dẫn | Ghi chú |
|---|---|---|
| POST | `/v1/auth/dev-login` | `{"user_id": 123}` → JWT 24 h. Chỉ bật khi `APP_ENV=dev` |
| GET | `/v1/events/{id}` | Thông tin sự kiện, các khu và giá |
| GET | `/v1/events/{id}/seats` | 5.000 ghế kèm trạng thái và `version`; cache 500 ms, gzip |
| POST | `/v1/orders` | Cần `Authorization` và `Idempotency-Key` (UUID). 201 tạo mới, 200 gửi lại, 409 `IDEMPOTENCY_KEY_IN_PROGRESS` khi request cùng key đang chạy |
| GET | `/v1/orders/{id}` | Chỉ chủ đơn xem được |
| POST | `/v1/orders/{id}/cancel` | Chỉ khi `HELD`; huỷ lại đơn đã huỷ vẫn trả 200 |
| GET | `/healthz`, `/readyz` | readyz kiểm tra PostgreSQL và Redis |

Lỗi luôn có dạng `{"error": {"code", "message", "details"}}`. Bảng mã lỗi ở [internal/httpx/errors.go](internal/httpx/errors.go).

## Chạy local

### Yêu cầu

- Go 1.27+
- Docker (trên macOS dùng OrbStack), Docker Compose v2
- `openssl`, `make`
- `sqlc` 1.31+, `goose` 3.x, `k6` (cài bằng `brew install sqlc goose k6`)

### Khởi động

```sh
make up            # postgres, redis, redpanda, redpanda console; chờ đến khi healthy
make migrate       # tạo schema
make seed          # sự kiện mẫu 5.000 ghế (sau make db-reset thì có id 1)
make run-booking   # lần đầu tự tạo .env từ .env.example với secret ngẫu nhiên
```

Thử một lượt mua:

```sh
TOKEN=$(curl -s -X POST localhost:8080/v1/auth/dev-login -d '{"user_id":123}' | jq -r .access_token)
curl -s -X POST localhost:8080/v1/orders \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"event_id":1,"seat_ids":["VIP-A-12","VIP-A-13"]}'
```

`/readyz` trả `200` khi kết nối được PostgreSQL và Redis, `503` nếu không (body chỉ ghi `ok`/`fail` cho từng phụ thuộc, lý do lỗi nằm trong log). `/healthz` luôn trả `200` khi process còn sống.

| Dịch vụ | Địa chỉ |
|---|---|
| Booking API | http://localhost:8080 |
| PostgreSQL | `localhost:5432` (user/pass/db: `ticketrush`) |
| Redis | `localhost:6379` |
| Redpanda (Kafka API) | `localhost:19092` |
| Redpanda Console | http://localhost:8088 |

Dừng hạ tầng bằng `make down` (volume dữ liệu được giữ lại). Xem mọi target bằng `make help`.

### Cấu hình

Mọi cấu hình qua biến môi trường, liệt kê trong [.env.example](.env.example). Service kiểm tra cấu hình khi khởi động và thoát kèm danh sách **tất cả** biến thiếu/sai trong một lần. Bốn secret (`JWT_SECRET`, `ADMISSION_SECRET`, `WEBHOOK_SECRET`, `TICKET_SECRET`) bắt buộc, dài tối thiểu 32 ký tự và phải khác nhau. File `.env` không được commit.

### Kiểm thử và lint

```sh
make test              # unit test, bật race detector
make test-integration  # test tích hợp với PostgreSQL thật (testcontainers, cần Docker)
make lint              # gofmt, go vet, staticcheck, sqlc diff
```

Test tích hợp chạy trên PostgreSQL và Redis thật. Mỗi test có một database riêng, clone từ template đã migrate, và một Redis trống. Các test tranh chấp chạy cho **cả hai backend** giữ ghế:
- 1.000 goroutine cùng giữ `VIP-A-1` thì đúng 1 thành công;
- 200 goroutine giữ ngẫu nhiên 2 ghế trong 20 ghế thì không ghế nào thuộc hai đơn;
- 10 request song song cùng Idempotency-Key thì chỉ ra 1 đơn, và không request nào nhận `SEATS_UNAVAILABLE`;
- luồng HTTP đầu–cuối.

Ngoài ra có test riêng cho từng script Lua.

CI (GitHub Actions, [.github/workflows/ci.yaml](.github/workflows/ci.yaml)) có hai job:
- lint + unit test: gofmt, `sqlc diff`, `go vet`, staticcheck, `go test -race`;
- test tích hợp.

### Test tải

```sh
make bench-hold BACKEND=redis   # đo chuẩn: 3 lần, tự reset DB + seed + chạy booking, in trung vị/min/max
make bench-hold BACKEND=pg      # bản gốc để so sánh

# Hoặc chạy một lần với booking đang chạy sẵn:
make db-reset seed && make run-booking   # terminal khác
make load-hold                           # BUYER_MODE=vu để thử chế độ mỗi VU một người mua
```

`make bench` và chaos (`make chaos-*`) có từ M7/M8; hiện các target này báo "chưa triển khai" và trả exit code 1.

## Tiến độ

| Milestone | Nội dung | Trạng thái |
|---|---|---|
| M0 | Khung dự án, hạ tầng local, healthz/readyz, CI | Xong |
| M1 | Bản gốc chỉ dùng PostgreSQL | Xong |
| M2 | Giữ ghế bằng Redis Lua | Xong |
| M3 | Expiry worker và outbox | Chưa làm |
| M4 | Fakepay, webhook, saga | Chưa làm |
| M5 | Phòng chờ ảo, frontend | Chưa làm |
| M6 | Quan sát hệ thống | Chưa làm |
| M7 | Test tải, kiểm tra bất biến | Chưa làm |
| M8 | Chaos testing | Chưa làm |
| M9 | Kubernetes, CI hoàn chỉnh | Chưa làm |

## Hạn chế đã biết

- Booking từ chối chạy với `REQUIRE_ADMISSION=true` cho đến khi có phòng chờ (M5).
- API tạo đơn cần Redis cho khoá idempotency, kể cả khi `INVENTORY_BACKEND=pg`. Redis không trả lời thì trả `503` ([ADR-005](docs/adr/005-idempotency-key.md)).
- Chưa có reconcile (M8). Nếu Redis mất dữ liệu, ghế đang giữ sẽ bị coi là trống trong Redis dù đơn vẫn `HELD`. Lớp chặn cuối chống bán trùng là `UNIQUE` của bảng `tickets` khi thanh toán (M4).
- Chưa có expiry worker (M3): đơn `HELD` quá hạn vẫn ở trạng thái `HELD` (dù ghế đã được giải phóng khi `seat_holds` hết hạn), nên người đó chưa tạo được đơn mới cho sự kiện cho đến khi tự huỷ đơn cũ.
- Outbox đã được ghi nhưng chưa có relay đẩy lên Kafka (M3).
- Chưa có thanh toán, nên response chưa có `payment_url` (M4).
- Sự kiện và ghế được cache trong process suốt vòng đời của nó; seed lại với sơ đồ khác thì phải khởi động lại booking.
- Cổng lắng nghe cố định theo SPEC (booking `:8080`), chưa cấu hình qua biến môi trường.
