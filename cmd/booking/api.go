package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/auth/authdb"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/ticket"
)

// seatMapTTL is how long a rendered seat map is served from memory
// (SPEC.md 7.1).
const seatMapTTL = 500 * time.Millisecond

// paymentLinkTimeout bounds the call to fakepay made while answering a
// create or get (SPEC.md 9.1 step 9).
const paymentLinkTimeout = 2 * time.Second

// paymentLinks registers payments with the gateway (payment.Client).
type paymentLinks interface {
	CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (string, error)
}

// apiDeps are the booking handlers' dependencies.
type apiDeps struct {
	logger   *slog.Logger
	db       authdb.DBTX
	tokens   *auth.Tokens
	catalog  *catalog.Service
	orders   *order.Service
	payments paymentLinks
	// webhookSecret verifies X-Signature; callbackURL is where fakepay posts.
	webhookSecret string
	callbackURL   string
	devLogin      bool
}

// api holds the booking HTTP handlers.
type api struct {
	apiDeps
	seatMaps *seatMapCache
}

func newAPI(d apiDeps) *api {
	a := &api{apiDeps: d}
	a.seatMaps = newSeatMapCache(seatMapTTL, a.renderSeatMap)
	return a
}

func (a *api) routes(r chi.Router) {
	r.Route("/v1", func(r chi.Router) {
		if a.devLogin {
			r.Post("/auth/dev-login", a.handleDevLogin)
		}
		r.Get("/events/{eventID}", a.handleGetEvent)
		r.Get("/events/{eventID}/seats", a.handleGetSeats)
		r.Post("/webhooks/fakepay", a.handleWebhook)

		r.Group(func(r chi.Router) {
			r.Use(httpx.RequireUser(a.tokens, a.logger))
			r.Post("/orders", a.handleCreateOrder)
			r.Get("/orders/{orderID}", a.handleGetOrder)
			r.Post("/orders/{orderID}/cancel", a.handleCancelOrder)
			r.Get("/orders/{orderID}/tickets", a.handleTickets)
		})
	})
}

// ---- auth ----------------------------------------------------------------

type devLoginRequest struct {
	UserID int64 `json:"user_id"`
}

type devLoginResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// handleDevLogin issues a token for any user id. Only mounted when APP_ENV=dev.
func (a *api) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	var req devLoginRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	if req.UserID <= 0 {
		httpx.WriteErr(w, r, a.logger, order.NewValidationError("user_id", "must be a positive integer"))
		return
	}
	if err := auth.EnsureUser(r.Context(), a.db, req.UserID); err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	token, err := a.tokens.Issue(req.UserID)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, devLoginResponse{AccessToken: token, ExpiresIn: int64(a.tokens.TTL().Seconds())})
}

// ---- events --------------------------------------------------------------

type zoneResponse struct {
	Zone     string `json:"zone"`
	PriceVND int64  `json:"price_vnd"`
	Seats    int    `json:"seats"`
}

type eventResponse struct {
	ID               int64          `json:"id"`
	Name             string         `json:"name"`
	Venue            string         `json:"venue"`
	StartsAt         time.Time      `json:"starts_at"`
	SaleOpensAt      time.Time      `json:"sale_opens_at"`
	MaxSeatsPerOrder int            `json:"max_seats_per_order"`
	Zones            []zoneResponse `json:"zones"`
}

func (a *api) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathEventID(r)
	if !ok {
		httpx.WriteErr(w, r, a.logger, catalog.ErrEventNotFound)
		return
	}
	ev, err := a.catalog.Event(r.Context(), eventID)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	resp := eventResponse{
		ID:               ev.ID,
		Name:             ev.Name,
		Venue:            ev.Venue,
		StartsAt:         ev.StartsAt.UTC(),
		SaleOpensAt:      ev.SaleOpensAt.UTC(),
		MaxSeatsPerOrder: ev.MaxSeatsPerOrder,
	}
	for _, z := range ev.Zones {
		resp.Zones = append(resp.Zones, zoneResponse{Zone: z.Name, PriceVND: z.PriceVND, Seats: z.Seats})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type seatResponse struct {
	SeatID   string `json:"seat_id"`
	Zone     string `json:"zone"`
	Row      string `json:"row"`
	No       int    `json:"no"`
	PriceVND int64  `json:"price_vnd"`
	Status   string `json:"status"`
}

type seatMapResponse struct {
	Version int64          `json:"version"`
	Seats   []seatResponse `json:"seats"`
}

func (a *api) handleGetSeats(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathEventID(r)
	if !ok {
		httpx.WriteErr(w, r, a.logger, catalog.ErrEventNotFound)
		return
	}
	entry, err := a.seatMaps.get(r.Context(), eventID)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	h := w.Header()
	h.Set("ETag", entry.etag)
	h.Set("Vary", "Accept-Encoding")
	// Clients may keep the map but must revalidate it before trusting it.
	h.Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), entry.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := entry.plain
	h.Set("Content-Type", "application/json; charset=utf-8")
	if acceptsGzip(r) {
		body = entry.gzipped
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// renderSeatMap is the seat map cache's loader.
func (a *api) renderSeatMap(ctx context.Context, eventID int64) (renderedSeatMap, error) {
	m, err := a.catalog.SeatMap(ctx, eventID)
	if err != nil {
		return renderedSeatMap{}, err
	}
	resp := seatMapResponse{Version: m.Version, Seats: make([]seatResponse, len(m.Seats))}
	for i, s := range m.Seats {
		resp.Seats[i] = seatResponse{SeatID: s.ID, Zone: s.Zone, Row: s.Row, No: s.No, PriceVND: s.PriceVND, Status: string(s.Status)}
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return renderedSeatMap{}, fmt.Errorf("encode seat map: %w", err)
	}
	etag := contentETag(body)
	if m.Versioned {
		etag = versionETag(m.Version)
	}
	return renderedSeatMap{body: body, etag: etag}, nil
}

// ---- orders --------------------------------------------------------------

type orderSeatResponse struct {
	SeatID   string `json:"seat_id"`
	PriceVND int64  `json:"price_vnd"`
}

type orderResponse struct {
	OrderID       uuid.UUID           `json:"order_id"`
	EventID       int64               `json:"event_id"`
	Status        order.Status        `json:"status"`
	TotalVND      int64               `json:"total_vnd"`
	HoldExpiresAt time.Time           `json:"hold_expires_at"`
	Seats         []orderSeatResponse `json:"seats"`
	// PaymentURL is where to pay; present only while the order is HELD.
	PaymentURL string `json:"payment_url,omitempty"`
	// Tickets are listed once issued (TICKETED), without the PNG images.
	Tickets   []ticketResponse `json:"tickets,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

func toOrderResponse(o *order.Order) orderResponse {
	resp := orderResponse{
		OrderID:       o.ID,
		EventID:       o.EventID,
		Status:        o.Status,
		TotalVND:      o.TotalVND,
		HoldExpiresAt: o.HoldExpiresAt.UTC(),
		CreatedAt:     o.CreatedAt.UTC(),
		Seats:         make([]orderSeatResponse, len(o.Seats)),
	}
	for i, s := range o.Seats {
		resp.Seats[i] = orderSeatResponse{SeatID: s.SeatID, PriceVND: s.PriceVND}
	}
	return resp
}

func (a *api) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFrom(r.Context())
	key, err := parseIdempotencyKey(r.Header.Get("Idempotency-Key"))
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	var req order.CreateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}

	o, created, err := a.orders.Create(r.Context(), userID, key, req)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	status := http.StatusOK // replay of an earlier request
	if created {
		status = http.StatusCreated
	}
	resp := toOrderResponse(o)
	resp.PaymentURL = a.paymentURL(r.Context(), o)
	httpx.WriteJSON(w, status, resp)
}

// paymentURL registers o with the gateway and returns where to pay. Only a
// HELD order can be paid. If the gateway is slow or down the order is still
// returned without a link; the client gets it later from GET /v1/orders/{id}.
func (a *api) paymentURL(ctx context.Context, o *order.Order) string {
	if a.payments == nil || o.Status != order.StatusHeld {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, paymentLinkTimeout)
	defer cancel()
	u, err := a.payments.CreatePayment(ctx, payment.CreatePaymentRequest{
		OrderID: o.ID, AmountVND: o.TotalVND, CallbackURL: a.callbackURL,
	})
	if err != nil {
		a.logger.WarnContext(ctx, "payment link unavailable; the client can fetch it later", "err", err)
		return ""
	}
	return u
}

func (a *api) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	a.withOrder(w, r, a.orders.Get)
}

// ---- payments and tickets ------------------------------------------------

// maxWebhookBytes caps the webhook body; the real one is ~200 bytes.
const maxWebhookBytes = 64 << 10

type webhookResponse struct {
	Outcome order.Outcome `json:"outcome"`
}

// handleWebhook receives fakepay's payment results (SPEC.md 7.1, 9.3). It
// answers 200 whenever the webhook is recorded (duplicates and refunds
// included), 401 for a bad signature and 500 only for infrastructure
// errors, which makes fakepay retry.
func (a *api) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		httpx.WriteErr(w, r, a.logger, fmt.Errorf("%w: %w", httpx.ErrBadRequest, err))
		return
	}
	// The signature covers the raw bytes, so check it before parsing.
	if !payment.Verify(a.webhookSecret, body, r.Header.Get(payment.SignatureHeader)) {
		httpx.WriteErr(w, r, a.logger, payment.ErrBadSignature)
		return
	}
	var wh payment.Webhook
	if err := json.Unmarshal(body, &wh); err != nil {
		httpx.WriteErr(w, r, a.logger, fmt.Errorf("%w: %w", payment.ErrInvalidWebhook, err))
		return
	}
	if err := wh.Validate(); err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	res, err := a.orders.ApplyPayment(r.Context(), wh)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, webhookResponse{Outcome: res.Outcome})
}

type ticketResponse struct {
	TicketID    uuid.UUID `json:"ticket_id"`
	SeatID      string    `json:"seat_id"`
	IssuedAt    time.Time `json:"issued_at"`
	QRPayload   string    `json:"qr_payload"`
	QRPNGBase64 string    `json:"qr_png_base64,omitempty"`
}

type ticketsResponse struct {
	OrderID uuid.UUID        `json:"order_id"`
	Tickets []ticketResponse `json:"tickets"`
}

// handleTickets returns the issued tickets of the caller's order with their
// QR codes as base64 PNG images. Before issuance the list is empty.
func (a *api) handleTickets(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFrom(r.Context())
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteErr(w, r, a.logger, order.ErrNotFound)
		return
	}
	ctx := logging.With(r.Context(), slog.String("order_id", orderID.String()))
	tickets, err := a.issuedTickets(ctx, userID, orderID, true)
	if err != nil {
		httpx.WriteErr(w, r.WithContext(ctx), a.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ticketsResponse{OrderID: orderID, Tickets: tickets})
}

// issuedTickets lists the order's issued tickets, with PNG QR codes if
// withPNG. Tickets not issued yet are left out.
func (a *api) issuedTickets(ctx context.Context, userID int64, orderID uuid.UUID, withPNG bool) ([]ticketResponse, error) {
	tickets, err := a.orders.Tickets(ctx, userID, orderID)
	if err != nil {
		return nil, err
	}
	out := []ticketResponse{}
	for _, t := range tickets {
		if t.QRPayload == "" || t.IssuedAt == nil {
			continue
		}
		tr := ticketResponse{TicketID: t.ID, SeatID: t.SeatID, IssuedAt: t.IssuedAt.UTC(), QRPayload: t.QRPayload}
		if withPNG {
			png, err := ticket.PNG(t.QRPayload)
			if err != nil {
				return nil, err
			}
			tr.QRPNGBase64 = base64.StdEncoding.EncodeToString(png)
		}
		out = append(out, tr)
	}
	return out, nil
}

func (a *api) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	a.withOrder(w, r, a.orders.Cancel)
}

// withOrder runs op on the order named in the path and writes it back.
func (a *api) withOrder(w http.ResponseWriter, r *http.Request, op func(context.Context, int64, uuid.UUID) (*order.Order, error)) {
	userID, _ := auth.UserIDFrom(r.Context())
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteErr(w, r, a.logger, order.ErrNotFound)
		return
	}
	ctx := logging.With(r.Context(), slog.String("order_id", orderID.String()))
	o, err := op(ctx, userID, orderID)
	if err != nil {
		httpx.WriteErr(w, r.WithContext(ctx), a.logger, err)
		return
	}
	resp := toOrderResponse(o)
	resp.PaymentURL = a.paymentURL(ctx, o)
	if o.Status == order.StatusTicketed {
		if resp.Tickets, err = a.issuedTickets(ctx, userID, orderID, false); err != nil {
			httpx.WriteErr(w, r.WithContext(ctx), a.logger, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ---- helpers -------------------------------------------------------------

func pathEventID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	return id, err == nil && id > 0
}

// parseIdempotencyKey requires a UUID (SPEC.md 7.1) and returns it in
// canonical form, so differently formatted spellings map to the same key.
func parseIdempotencyKey(raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, order.NewValidationError("Idempotency-Key", "header is required")
	}
	key, err := uuid.Parse(raw)
	if err != nil || key == uuid.Nil {
		return uuid.Nil, order.NewValidationError("Idempotency-Key", "must be a UUID")
	}
	return key, nil
}
