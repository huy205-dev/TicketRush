// Package fakepay is a fake payment gateway (SPEC.md 7.3). It stands in for
// an external provider, so it keeps its state in memory and misbehaves on
// purpose: webhooks arrive after a random delay, sometimes very late,
// sometimes several times, and are sometimes resent even though booking
// accepted them.
package fakepay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/money"
	"github.com/huy205-dev/ticketrush/internal/payment"
)

// Config tunes the gateway. The zero value of a rate disables that fault.
type Config struct {
	WebhookSecret string
	PublicURL     string
	DelayMin      time.Duration
	DelayMax      time.Duration
	DuplicateRate float64
	LateRate      float64
	LateExtra     time.Duration
	FailRate      float64
	// MaxResends and RetryBase shape redelivery after the first post:
	// waits of RetryBase, 2x, 4x... for at most MaxResends resends (SPEC.md
	// 7.3: 1s, 2s, 4s... at most 8 times).
	MaxResends int
	RetryBase  time.Duration
	// CallbackPrefix is booking's base URL: callback URLs outside it are
	// refused, so fakepay cannot be made to post signed requests anywhere
	// else. Empty allows any URL (tests).
	CallbackPrefix string
}

// ErrInvalidRequest is wrapped by every validation error; handlers answer
// 400.
var ErrInvalidRequest = errors.New("invalid request")

// Server is the fake gateway.
type Server struct {
	cfg    Config
	logger *slog.Logger
	client *http.Client
	// ctx is the lifetime of background deliveries, which outlive the HTTP
	// request that scheduled them. It is passed explicitly to every function
	// doing I/O; cancelling it stops all deliveries.
	ctx context.Context
	wg  sync.WaitGroup

	mu       sync.Mutex
	rnd      *mrand.Rand
	payments map[uuid.UUID]payment.CreatePaymentRequest
	refunds  map[string]payment.RefundRequest
	txns     map[string]uuid.UUID // provider_txn_id -> order
}

// New returns a gateway whose webhook deliveries run until ctx is cancelled.
func New(ctx context.Context, cfg Config, logger *slog.Logger) *Server {
	if cfg.MaxResends <= 0 {
		cfg.MaxResends = 8
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	return &Server{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// A redirect would re-send the signed body somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ctx:      ctx,
		rnd:      mrand.New(mrand.NewPCG(uint64(time.Now().UnixNano()), 0x7469636b6574)),
		payments: make(map[uuid.UUID]payment.CreatePaymentRequest),
		refunds:  make(map[string]payment.RefundRequest),
		txns:     make(map[string]uuid.UUID),
	}
}

// Wait blocks until every scheduled webhook delivery has finished or given
// up (after the context passed to New is cancelled).
func (s *Server) Wait() { s.wg.Wait() }

// Routes mounts the gateway's endpoints.
func (s *Server) Routes(r chi.Router) {
	r.Post("/v1/payments", s.handleCreate)
	r.Post("/v1/payments/auto", s.handleAuto)
	r.Post("/v1/refunds", s.handleRefund)
	r.Get("/pay/{orderID}", s.handlePage)
	r.Post("/pay/{orderID}/complete", s.handleComplete)
}

// ---- payments ------------------------------------------------------------

func (s *Server) validCreate(req payment.CreatePaymentRequest) error {
	u, err := url.Parse(req.CallbackURL)
	prefix := s.cfg.CallbackPrefix
	switch {
	case req.OrderID == uuid.Nil:
		return fmt.Errorf("%w: order_id is required", ErrInvalidRequest)
	case req.AmountVND <= 0:
		return fmt.Errorf("%w: amount_vnd must be positive", ErrInvalidRequest)
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return fmt.Errorf("%w: callback_url must be an absolute http(s) URL", ErrInvalidRequest)
	case prefix != "" && req.CallbackURL != prefix && !strings.HasPrefix(req.CallbackURL, prefix+"/"):
		return fmt.Errorf("%w: callback_url must be under %s", ErrInvalidRequest, prefix)
	}
	return nil
}

// register stores the payment and returns the registration in force:
// registering an order again keeps the first one, so the call is idempotent.
func (s *Server) register(req payment.CreatePaymentRequest) payment.CreatePaymentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.payments[req.OrderID]; ok {
		return p
	}
	s.payments[req.OrderID] = req
	return req
}

func (s *Server) paymentURL(orderID uuid.UUID) string {
	return s.cfg.PublicURL + "/pay/" + orderID.String()
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req payment.CreatePaymentRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := s.validCreate(req); err != nil {
		badRequest(w, err)
		return
	}
	p := s.register(req)
	httpx.WriteJSON(w, http.StatusOK, payment.CreatePaymentResponse{PaymentURL: s.paymentURL(p.OrderID)})
}

type completeRequest struct {
	Result string `json:"result"`
}

type completeResponse struct {
	ProviderTxnID string `json:"provider_txn_id"`
}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, httpx.ErrorBody{Code: "PAYMENT_NOT_FOUND", Message: "Không có thanh toán cho đơn này"})
		return
	}
	var req completeRequest
	if !s.decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	p, ok := s.payments[orderID]
	s.mu.Unlock()
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, httpx.ErrorBody{Code: "PAYMENT_NOT_FOUND", Message: "Không có thanh toán cho đơn này"})
		return
	}
	txn, err := s.complete(p, req.Result)
	if err != nil {
		badRequest(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, completeResponse{ProviderTxnID: txn})
}

type autoRequest struct {
	payment.CreatePaymentRequest
	// Result defaults to SUCCEEDED.
	Result string `json:"result"`
}

// handleAuto registers and completes a payment in one call, for load tests
// that have no browser to click the page.
func (s *Server) handleAuto(w http.ResponseWriter, r *http.Request) {
	var req autoRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.Result == "" {
		req.Result = payment.StatusSucceeded
	}
	if err := s.validCreate(req.CreatePaymentRequest); err != nil {
		badRequest(w, err)
		return
	}
	// An order registered before keeps its amount and callback.
	p := s.register(req.CreatePaymentRequest)
	txn, err := s.complete(p, req.Result)
	if err != nil {
		badRequest(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, completeResponse{ProviderTxnID: txn})
}

// complete creates a new transaction for p and schedules its webhook. Each
// call is a separate transaction, so paying twice means two payments.
func (s *Server) complete(p payment.CreatePaymentRequest, result string) (string, error) {
	if result != payment.StatusSucceeded && result != payment.StatusFailed {
		return "", fmt.Errorf("%w: result must be SUCCEEDED or FAILED", ErrInvalidRequest)
	}
	txn := "fp_" + rand.Text()
	s.mu.Lock()
	s.txns[txn] = p.OrderID
	s.mu.Unlock()

	body, err := json.Marshal(payment.Webhook{
		ProviderTxnID: txn, OrderID: p.OrderID, AmountVND: p.AmountVND, Status: result, PaidAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	s.wg.Go(func() { s.deliver(s.ctx, p, txn, body) })
	return txn, nil
}

// ---- webhook delivery ----------------------------------------------------

// deliver waits the configured delay, then sends the webhook, plus 1-3
// duplicates with probability DuplicateRate, all at once.
func (s *Server) deliver(ctx context.Context, p payment.CreatePaymentRequest, txn string, body []byte) {
	s.mu.Lock()
	delay := s.cfg.DelayMin
	if span := s.cfg.DelayMax - s.cfg.DelayMin; span > 0 {
		delay += time.Duration(s.rnd.Int64N(int64(span) + 1))
	}
	late := s.rnd.Float64() < s.cfg.LateRate
	if late {
		delay += s.cfg.LateExtra
	}
	copies := 1
	if s.rnd.Float64() < s.cfg.DuplicateRate {
		copies += 1 + s.rnd.IntN(3)
	}
	s.mu.Unlock()

	logger := s.logger.With("order_id", p.OrderID.String(), "provider_txn_id", txn)
	logger.Info("webhook scheduled", "delay_ms", delay.Milliseconds(), "late", late, "copies", copies)
	if !s.sleep(ctx, delay) {
		logger.Warn("webhook delivery cancelled by shutdown", "posts", 0)
		return
	}
	sig := payment.Sign(s.cfg.WebhookSecret, body)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Go(func() { s.sendWithRetry(ctx, logger.With("copy", i+1), p.CallbackURL, body, sig) })
	}
	wg.Wait()
}

// sendWithRetry posts the webhook until booking answers 200 and fakepay does
// not decide to count it as a failure anyway, then resends with exponential
// backoff, at most MaxResends times.
func (s *Server) sendWithRetry(ctx context.Context, logger *slog.Logger, callback string, body []byte, sig string) {
	wait := s.cfg.RetryBase
	for posts := 1; ; posts++ {
		status, err := s.post(ctx, callback, body, sig)
		s.mu.Lock()
		forcedFailure := err == nil && status == http.StatusOK && s.rnd.Float64() < s.cfg.FailRate
		s.mu.Unlock()
		switch {
		case err == nil && status == http.StatusOK && !forcedFailure:
			logger.Info("webhook delivered", "posts", posts)
			return
		case forcedFailure:
			logger.Info("webhook accepted but counted as failed on purpose", "posts", posts)
		default:
			logger.Warn("webhook delivery failed", "posts", posts, "status", status, "err", err)
		}
		if posts > s.cfg.MaxResends {
			logger.Error("webhook given up", "posts", posts)
			return
		}
		if !s.sleep(ctx, wait) {
			logger.Warn("webhook delivery cancelled by shutdown", "posts", posts)
			return
		}
		wait *= 2
	}
}

func (s *Server) post(ctx context.Context, callback string, body []byte, sig string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(payment.SignatureHeader, sig)
	res, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = res.Body.Close()
	return res.StatusCode, nil
}

// sleep waits d unless ctx ends first; it reports whether the full wait
// elapsed.
func (s *Server) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---- refunds -------------------------------------------------------------

// handleRefund refunds a transaction, idempotently by provider_txn_id.
func (s *Server) handleRefund(w http.ResponseWriter, r *http.Request) {
	var req payment.RefundRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.ProviderTxnID == "" || req.OrderID == uuid.Nil || req.AmountVND <= 0 {
		badRequest(w, fmt.Errorf("%w: order_id, provider_txn_id and a positive amount_vnd are required", ErrInvalidRequest))
		return
	}
	s.mu.Lock()
	_, known := s.txns[req.ProviderTxnID]
	_, repeat := s.refunds[req.ProviderTxnID]
	if !repeat {
		s.refunds[req.ProviderTxnID] = req
	}
	s.mu.Unlock()
	if !known {
		// State is in memory; after a restart fakepay no longer knows the
		// transaction but still honours the refund.
		s.logger.Warn("refund for a transaction this process did not create",
			"order_id", req.OrderID.String(), "provider_txn_id", req.ProviderTxnID)
	}
	s.logger.Info("refund", "order_id", req.OrderID.String(), "provider_txn_id", req.ProviderTxnID,
		"amount_vnd", req.AmountVND, "repeat", repeat)
	httpx.WriteJSON(w, http.StatusOK, payment.RefundResponse{Status: payment.StatusSucceeded})
}

// Refunds returns how many distinct transactions were refunded (for tests).
func (s *Server) Refunds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refunds)
}

// ---- payment page --------------------------------------------------------

var page = template.Must(template.New("pay").Parse(`<!doctype html>
<html lang="vi">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Fakepay – thanh toán</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem;color:#1a1a1a}
.amount{font-size:2rem;font-weight:600;margin:.5rem 0 1.5rem}
button{font-size:1rem;padding:.75rem 1.25rem;margin-right:.5rem;border-radius:.5rem;border:1px solid #888;cursor:pointer}
.ok{background:#1f7a3d;color:#fff;border-color:#1f7a3d}
#result{margin-top:1.5rem}
</style>
</head>
<body>
<h1>Fakepay</h1>
<p>Cổng thanh toán giả lập. Đơn hàng <code>{{.OrderID}}</code></p>
<div class="amount">{{.Amount}} đ</div>
<button class="ok" onclick="pay('SUCCEEDED')">Thanh toán thành công</button>
<button onclick="pay('FAILED')">Thanh toán thất bại</button>
<p id="result"></p>
<script>
async function pay(result) {
  const out = document.getElementById('result');
  const res = await fetch(location.pathname + '/complete', {
    method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({result})
  });
  const body = await res.json();
  out.textContent = res.ok
    ? 'Đã ghi nhận (' + result + '), mã giao dịch ' + body.provider_txn_id + '. Kết quả sẽ được báo về trang bán vé.'
    : 'Lỗi: ' + (body.error ? body.error.message : res.status);
}
</script>
</body>
</html>`))

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	s.mu.Lock()
	p, ok := s.payments[orderID]
	s.mu.Unlock()
	if err != nil || !ok {
		http.Error(w, "Không có thanh toán cho đơn này", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, struct {
		OrderID string
		Amount  string
	}{p.OrderID.String(), money.FormatVND(p.AmountVND)})
}

// ---- helpers -------------------------------------------------------------

func (s *Server) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := httpx.DecodeJSON(r, dst); err != nil {
		badRequest(w, err)
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, err error) {
	httpx.WriteError(w, http.StatusBadRequest, httpx.ErrorBody{Code: "BAD_REQUEST", Message: err.Error()})
}
