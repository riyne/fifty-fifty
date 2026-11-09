// Package payclient talks to the payment provider (in this project, the mock in
// internal/harness/mockpay; settlement is simulated). It has its own wire types and imports
// nothing from the mock, so the code that talks to the mock is the code that would
// talk to a real provider.
//
// What it is careful about is classifying a result, because that decides what happens
// to money:
//
//   - A payment came back (status succeeded or failed): a definite answer.
//   - ErrRejected: the provider refused the request itself (a 4xx such as 400 or 422).
//     That is a bug on our side, not a payment outcome, and retrying will not help.
//   - ErrAmbiguous: we do not know whether the payment happened (a timeout, a dropped
//     connection, a 5xx, a body we cannot understand, 408/429). A 5xx can come after the
//     provider has already taken the payment, so it is NOT a failure.
//   - ErrNotFound (Get only): the provider has no payment for the key.
//   - The caller's context ending is none of these: it is returned as is, because
//     cancellation says nothing about the payment.
package payclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrRejected  = errors.New("payclient: request rejected by the provider")
	ErrAmbiguous = errors.New("payclient: outcome unknown")
	ErrNotFound  = errors.New("payclient: no payment for this key")
)

// maxResponseBytes bounds what is read from the provider.
const maxResponseBytes = 1 << 16

// Statuses of a payment as the provider reports them.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Request is a payment to create.
type Request struct {
	Reference   string `json:"reference"`
	From        string `json:"from"`
	To          string `json:"to"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// Payment is the provider's record of a payment.
type Payment struct {
	ID             string  `json:"id"`
	IdempotencyKey string  `json:"idempotency_key"`
	Reference      string  `json:"reference"`
	AmountMinor    int64   `json:"amount_minor"`
	Currency       string  `json:"currency"`
	Status         string  `json:"status"`
	FailureCode    *string `json:"failure_code"`
}

// Client calls the provider at BaseURL.
type Client struct {
	BaseURL string
	// HTTP is the underlying client; nil means one with a 10 s timeout. Its timeout
	// bounds a whole call, and running out of it is ambiguous, not a failure.
	HTTP *http.Client
}

// New validates baseURL (an http or https URL) and returns a client.
func New(baseURL string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("payclient: %q is not an http(s) URL", baseURL)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Create asks the provider to create the payment for key. Calling it again with the
// same key and request returns the same payment and charges nothing, which is what
// makes a retry after an ambiguous result safe.
func (c *Client) Create(ctx context.Context, key string, r Request) (Payment, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return Payment{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/payments", bytes.NewReader(body))
	if err != nil {
		return Payment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	status, raw, err := c.do(ctx, req)
	if err != nil {
		return Payment{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		return parse(raw, key)
	default:
		return Payment{}, classify(status, raw)
	}
}

// Get reads the payment for key. ErrNotFound means the provider has none; ErrAmbiguous
// means this read failed (the answer is still unknown).
func (c *Client) Get(ctx context.Context, key string) (Payment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/payments?idempotency_key="+url.QueryEscape(key), nil)
	if err != nil {
		return Payment{}, err
	}
	status, raw, err := c.do(ctx, req)
	if err != nil {
		return Payment{}, err
	}
	switch status {
	case http.StatusOK:
		return parse(raw, key)
	case http.StatusNotFound:
		return Payment{}, ErrNotFound
	default:
		return Payment{}, classify(status, raw)
	}
}

// do sends the request and reads the body. A transport failure is ambiguous unless the
// caller's own context ended, which is returned as is.
func (c *Client) do(ctx context.Context, req *http.Request) (int, []byte, error) {
	resp, err := c.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("%w: %v", ErrAmbiguous, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("%w: reading the response: %v", ErrAmbiguous, err)
	}
	return resp.StatusCode, raw, nil
}

// classify turns a non-success status into an error. 408 and 429 mean the provider did
// not process the request, 5xx means it may or may not have: all ambiguous from here.
// Any other 4xx means the request itself is wrong.
func classify(status int, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return fmt.Errorf("%w: status %d", ErrAmbiguous, status)
	}
	return fmt.Errorf("%w: status %d: %s", ErrRejected, status, msg)
}

// parse accepts only a payment that is for this key and has a final status. Anything
// else, even with a 200, is not an answer we can act on.
func parse(raw []byte, key string) (Payment, error) {
	var p Payment
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payment{}, fmt.Errorf("%w: unreadable payment: %v", ErrAmbiguous, err)
	}
	if p.IdempotencyKey != key {
		return Payment{}, fmt.Errorf("%w: the payment is for another key", ErrAmbiguous)
	}
	if p.Status != StatusSucceeded && p.Status != StatusFailed {
		return Payment{}, fmt.Errorf("%w: payment status %q is not final", ErrAmbiguous, p.Status)
	}
	return p, nil
}
