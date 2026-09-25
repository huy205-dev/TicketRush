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
	"fmt"
	"html/template"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
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
	// MaxAttempts and RetryBase shape redelivery: RetryBase, 2x, 4x...
	// (SPEC.md: 1s, 2s, 4s... at most 8 attempts).
	MaxAttempts int
	RetryBase   time.Duration
}

// Server is the fake gateway.
type Server struct {
	cfg    Config
	logger *slog.Logger
	client *http.Client
	ctx    context.Context // deliveries stop when it is cancelled
	wg     sync.WaitGroup

	mu       sync.Mutex
	rnd      *mrand.Rand
	payments map[uuid.UUID]payment.CreatePaymentRequest
	refunds  map[string]payment.RefundRequest
	txns     map[string]uuid.UUID // provider_txn_id -> order
}

// New returns a gateway whose webhook deliveries run until ctx is cancelled.
func New(ctx context.Context, cfg Config, logger *slog.Logger) *Server {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	return &Server{
		cfg:      cfg,
		logger:   logger,
		client:   &http.Client{Timeout: 10 * time.Second},
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

func validCreate(req payment.CreatePaymentRequest) error {
	u, err := url.Parse(req.CallbackURL)
	switch {
	case req.OrderID == uuid.Nil:
		return fmt.Errorf("order_id is required")
	case req.AmountVND <= 0:
		return fmt.Errorf("amount_vnd must be positive")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return fmt.Errorf("callback_url must be an absolute http(s) URL")
	}
	return nil
}

// register stores the payment; registering an order again keeps the first
// registration, so the call is idempotent.
func (s *Server) register(req payment.CreatePaymentRequest) string {
	s.mu.Lock()
	if _, ok := s.payments[req.OrderID]; !ok {
		s.payments[req.OrderID] = req
	}
	s.mu.Unlock()
	return s.cfg.PublicURL + "/pay/" + req.OrderID.String()
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req payment.CreatePaymentRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := validCreate(req); err != nil {
		badRequest(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, payment.CreatePaymentResponse{PaymentURL: s.register(req)})
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
	if err := validCreate(req.CreatePaymentRequest); err != nil {
		badRequest(w, err)
		return
	}
	s.register(req.CreatePaymentRequest)
	txn, err := s.complete(req.CreatePaymentRequest, req.Result)
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
		return "", fmt.Errorf("result must be SUCCEEDED or FAILED")
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
	s.wg.Go(func() { s.deliver(txn, p.CallbackURL, body) })
	return txn, nil
}

// ---- webhook delivery ----------------------------------------------------

// deliver waits the configured delay, then sends the webhook, plus 1-3
// duplicates with probability DuplicateRate, all at once.
func (s *Server) deliver(txn, callback string, body []byte) {
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

	logger := s.logger.With("provider_txn_id", txn)
	logger.Info("webhook scheduled", "delay_ms", delay.Milliseconds(), "late", late, "copies", copies)
	if !s.sleep(delay) {
		return
	}
	sig := payment.Sign(s.cfg.WebhookSecret, body)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Go(func() { s.sendWithRetry(logger.With("copy", i+1), callback, body, sig) })
	}
	wg.Wait()
}

// sendWithRetry posts the webhook until booking answers 200 and fakepay does
// not decide to count it as a failure anyway, backing off exponentially.
func (s *Server) sendWithRetry(logger *slog.Logger, callback string, body []byte, sig string) {
	wait := s.cfg.RetryBase
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		status, err := s.post(callback, body, sig)
		s.mu.Lock()
		forcedFailure := err == nil && status == http.StatusOK && s.rnd.Float64() < s.cfg.FailRate
		s.mu.Unlock()
		switch {
		case err == nil && status == http.StatusOK && !forcedFailure:
			logger.Info("webhook delivered", "attempt", attempt)
			return
		case forcedFailure:
			logger.Info("webhook accepted but counted as failed on purpose", "attempt", attempt)
		default:
			logger.Warn("webhook delivery failed", "attempt", attempt, "status", status, "err", err)
		}
		if attempt == s.cfg.MaxAttempts || !s.sleep(wait) {
			break
		}
		wait *= 2
	}
	logger.Error("webhook given up", "attempts", s.cfg.MaxAttempts)
}

func (s *Server) post(callback string, body []byte, sig string) (int, error) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, callback, bytes.NewReader(body))
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

// sleep waits d unless the gateway is shutting down; it reports whether the
// full wait elapsed.
func (s *Server) sleep(d time.Duration) bool {
	if d <= 0 {
		return s.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
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
		badRequest(w, fmt.Errorf("order_id, provider_txn_id and a positive amount_vnd are required"))
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
		s.logger.Warn("refund for a transaction this process did not create", "provider_txn_id", req.ProviderTxnID)
	}
	s.logger.Info("refund", "provider_txn_id", req.ProviderTxnID, "amount_vnd", req.AmountVND, "repeat", repeat)
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
