package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIClient talks to the ChatGPT backend. It has NO CPA awareness — inputs
// are CodexCredentials, outputs are plain structs. BaseURL defaults to
// https://chatgpt.com and is only overridable for tests via httptest.Server.
type OpenAIClient struct {
	BaseURL string
	HTTP    *http.Client
}

const defaultBaseURL = "https://chatgpt.com"

// openAIDefaultHTTP is the fallback client used when OpenAIClient.HTTP is nil.
// A finite Timeout is mandatory: the worker goroutine drives one FSM per
// account synchronously, so a hung TCP connection against chatgpt.com would
// otherwise park that account permanently (spec §3.3, §6.1).
var openAIDefaultHTTP = &http.Client{Timeout: 30 * time.Second}

// HTTPStatusError carries the status code of a non-2xx response so callers
// (notably the retry classifier in retry.go) can distinguish 4xx (hard stop)
// from 5xx (retry). Body is included for diagnostics/logging.
type HTTPStatusError struct {
	Method string
	Path   string
	Code   int
	Body   []byte
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("openai %s %s: status %d", e.Method, e.Path, e.Code)
}

// IsClientError reports whether err is a 4xx HTTPStatusError (bad request,
// bad token, etc.) — such errors should NOT be retried.
func IsClientError(err error) bool {
	var se *HTTPStatusError
	return errors.As(err, &se) && se.Code >= 400 && se.Code < 500
}

func (c *OpenAIClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

func (c *OpenAIClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return openAIDefaultHTTP
}

func (c *OpenAIClient) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 1 MiB cap; the largest legitimate response from these endpoints is well
	// under 2 KiB. A larger body signals a buggy or hostile server.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, &HTTPStatusError{
			Method: req.Method,
			Path:   req.URL.Path,
			Code:   resp.StatusCode,
			Body:   body,
		}
	}
	return body, nil
}

func (c *OpenAIClient) setAuthHeaders(req *http.Request, creds CodexCredentials) {
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.ChatGPTAccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", creds.ChatGPTAccountID)
	}
	req.Header.Set("User-Agent", codexUserAgent)
}

// ListCredits calls GET /backend-api/wham/rate-limit-reset-credits.
func (c *OpenAIClient) ListCredits(creds CodexCredentials) (Snapshot, error) {
	u := c.baseURL() + resetCreditsEndpointPath
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return Snapshot{}, err
	}
	c.setAuthHeaders(req, creds)
	req.Header.Set("Accept", "application/json")
	body, err := c.do(req)
	if err != nil {
		return Snapshot{}, err
	}
	return parseResetCreditsResponse(body, time.Now())
}

// GetUsage calls GET /backend-api/wham/usage.
func (c *OpenAIClient) GetUsage(creds CodexCredentials) (Snapshot, error) {
	u := c.baseURL() + usageEndpointPath
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return Snapshot{}, err
	}
	c.setAuthHeaders(req, creds)
	req.Header.Set("Accept", "application/json")
	body, err := c.do(req)
	if err != nil {
		return Snapshot{}, err
	}
	return parseUsageResponse(body, time.Now())
}

// Consume calls POST /backend-api/wham/rate-limit-reset-credits/consume with
// the TARGETED form (spec §2.1): body always carries both redeem_request_id
// AND credit_id. The credit_id points at the specific soonest-expiring credit,
// so OpenAI cannot auto-consume a later one. The body has EXACTLY these two
// fields — the payload struct below has two non-omitempty fields, so
// json.Marshal cannot add or drop either.
//
// Both arguments MUST be non-empty: an empty credit_id silently degrades to
// OpenAI's auto-pick form (defeating the safety mechanism), and an empty
// redeem_request_id defeats server-side idempotency dedup. Validate before
// calling.
func (c *OpenAIClient) Consume(creds CodexCredentials, redeemRequestID, creditID string) (ConsumeResponse, error) {
	if redeemRequestID == "" || creditID == "" {
		return ConsumeResponse{}, errors.New("consume: redeem_request_id and credit_id are both required (empty defeats §2.1 safety)")
	}
	u := c.baseURL() + consumeEndpointPath
	payload := struct {
		RedeemRequestID string `json:"redeem_request_id"`
		CreditID        string `json:"credit_id"`
	}{RedeemRequestID: redeemRequestID, CreditID: creditID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ConsumeResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return ConsumeResponse{}, err
	}
	c.setAuthHeaders(req, creds)
	req.Header.Set("Content-Type", "application/json")
	body, err := c.do(req)
	if err != nil {
		return ConsumeResponse{}, err
	}
	return parseConsumeResponse(body)
}

// Endpoint paths are constants relative to BaseURL. The production BaseURL is
// https://chatgpt.com, so the full URL matches the documented contract in
// constants.go exactly (verified by TestFullURLEqualsDefaultBasePlusPath).
const (
	usageEndpointPath        = "/backend-api/wham/usage"
	resetCreditsEndpointPath = "/backend-api/wham/rate-limit-reset-credits"
	consumeEndpointPath      = "/backend-api/wham/rate-limit-reset-credits/consume"
)
