package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// seatMapTTL is how long a rendered seat map is served from memory
// (SPEC.md 7.1).
const seatMapTTL = 500 * time.Millisecond

// api holds the booking HTTP handlers.
type api struct {
	logger   *slog.Logger
	db       authdb.DBTX
	tokens   *auth.Tokens
	catalog  *catalog.Service
	orders   *order.Service
	seatMaps *seatMapCache
	devLogin bool
}

func newAPI(logger *slog.Logger, db authdb.DBTX, tokens *auth.Tokens, cat *catalog.Service, orders *order.Service, devLogin bool) *api {
	a := &api{logger: logger, db: db, tokens: tokens, catalog: cat, orders: orders, devLogin: devLogin}
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

		r.Group(func(r chi.Router) {
			r.Use(httpx.RequireUser(a.tokens, a.logger))
			r.Post("/orders", a.handleCreateOrder)
			r.Get("/orders/{orderID}", a.handleGetOrder)
			r.Post("/orders/{orderID}/cancel", a.handleCancelOrder)
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
	// PaymentURL is filled once fakepay exists (M4).
	PaymentURL string    `json:"payment_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
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
	httpx.WriteJSON(w, status, toOrderResponse(o))
}

func (a *api) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	a.withOrder(w, r, a.orders.Get)
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
	httpx.WriteJSON(w, http.StatusOK, toOrderResponse(o))
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
