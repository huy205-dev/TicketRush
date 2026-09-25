package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// CreatePaymentRequest registers a payment for an order (POST /v1/payments).
type CreatePaymentRequest struct {
	OrderID     uuid.UUID `json:"order_id"`
	AmountVND   int64     `json:"amount_vnd"`
	CallbackURL string    `json:"callback_url"`
}

// CreatePaymentResponse is fakepay's answer: where the buyer pays.
type CreatePaymentResponse struct {
	PaymentURL string `json:"payment_url"`
}

// RefundRequest asks fakepay to refund one transaction (POST /v1/refunds).
// Fakepay treats it idempotently by provider_txn_id.
type RefundRequest struct {
	OrderID       uuid.UUID `json:"order_id"`
	ProviderTxnID string    `json:"provider_txn_id"`
	AmountVND     int64     `json:"amount_vnd"`
}

// RefundResponse confirms a refund.
type RefundResponse struct {
	Status string `json:"status"`
}

// Client calls fakepay.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a client for fakepay at baseURL. Every call is bounded
// by timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}}
}

// CreatePayment registers the payment and returns the page where the buyer
// pays. Registering the same order again returns the same page.
func (c *Client) CreatePayment(ctx context.Context, req CreatePaymentRequest) (string, error) {
	var resp CreatePaymentResponse
	if err := c.post(ctx, "/v1/payments", req, &resp); err != nil {
		return "", fmt.Errorf("fakepay create payment: %w", err)
	}
	return resp.PaymentURL, nil
}

// Refund refunds one transaction. Calling it again for the same transaction
// is safe.
func (c *Client) Refund(ctx context.Context, req RefundRequest) error {
	var resp RefundResponse
	if err := c.post(ctx, "/v1/refunds", req, &resp); err != nil {
		return fmt.Errorf("fakepay refund %s: %w", req.ProviderTxnID, err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", res.StatusCode, bytes.TrimSpace(raw))
	}
	return json.Unmarshal(raw, out)
}
