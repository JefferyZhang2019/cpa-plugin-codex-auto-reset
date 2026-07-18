package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.Handler) (*OpenAIClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &OpenAIClient{
		BaseURL: srv.URL, // overrides hardcoded endpoints for testing
		HTTP:    srv.Client(),
	}, srv
}

func TestListCredits_WireFormat(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotAcct, gotUA string
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("Chatgpt-Account-Id")
		gotUA = r.Header.Get("User-Agent")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"credits":[],"available_count":0}`))
	}))
	snap, err := cli.ListCredits(CodexCredentials{AccessToken: "tok", ChatGPTAccountID: "acct-1"})
	if err != nil {
		t.Fatalf("ListCredits: %v", err)
	}
	if snap.AvailableCount != 0 {
		t.Fatalf("snap = %+v", snap)
	}
	if gotMethod != "GET" || gotPath != "/backend-api/wham/rate-limit-reset-credits" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotAcct != "acct-1" {
		t.Fatalf("account = %q", gotAcct)
	}
	if !strings.HasPrefix(gotUA, "codex_cli_rs/") {
		t.Fatalf("UA = %q", gotUA)
	}
}

func TestGetUsage_WireFormat(t *testing.T) {
	var gotMethod, gotPath string
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"plan_type":"plus"}`))
	}))
	_, err := cli.GetUsage(CodexCredentials{AccessToken: "t"})
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/backend-api/wham/usage" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
}

func TestConsume_WireFormat_TargetedBody(t *testing.T) {
	var gotBody map[string]any
	var gotCT string
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"code":"reset","windows_reset":2}`))
	}))
	resp, err := cli.Consume(CodexCredentials{AccessToken: "t", ChatGPTAccountID: "a"}, "rrid-123", "credit-456")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if resp.Code != ConsumeCodeReset || resp.WindowsReset != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	// Byte-exact body assertion.
	if gotBody["redeem_request_id"] != "rrid-123" {
		t.Fatalf("redeem_request_id = %v", gotBody["redeem_request_id"])
	}
	if gotBody["credit_id"] != "credit-456" {
		t.Fatalf("credit_id = %v", gotBody["credit_id"])
	}
	if len(gotBody) != 2 {
		t.Fatalf("body has extra fields: %+v", gotBody)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
}

func TestConsume_HTTPErrorStatus(t *testing.T) {
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream down`))
	}))
	_, err := cli.Consume(CodexCredentials{AccessToken: "t"}, "rrid", "cid")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error should mention status 502: %v", err)
	}
	// Must be a typed HTTPStatusError so retry.go can classify.
	var se *HTTPStatusError
	if !errors.As(err, &se) {
		t.Fatalf("error must be *HTTPStatusError, got %T: %v", err, err)
	}
	if se.Code != 502 {
		t.Fatalf("HTTPStatusError.Code = %d", se.Code)
	}
}

func TestConsume_EmptyArgsRejected(t *testing.T) {
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not be called when args are empty")
	}))
	cases := []struct{ name, rrid, cid string }{
		{"both empty", "", ""},
		{"empty credit_id", "rrid", ""},
		{"empty redeem_request_id", "", "cid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cli.Consume(CodexCredentials{AccessToken: "t"}, tc.rrid, tc.cid)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestIsClientError_Classifies4xxVs5xx(t *testing.T) {
	// 4xx -> hard stop (true); 5xx -> retry (false); network err -> retry (false).
	cli4xx, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	cli5xx, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	_, err4 := cli4xx.Consume(CodexCredentials{AccessToken: "t"}, "rrid", "cid")
	_, err5 := cli5xx.Consume(CodexCredentials{AccessToken: "t"}, "rrid", "cid")
	if !IsClientError(err4) {
		t.Fatalf("401 should be client error (hard stop): %v", err4)
	}
	if IsClientError(err5) {
		t.Fatalf("502 should NOT be client error (retry): %v", err5)
	}
	if IsClientError(errors.New("some network dial error")) {
		t.Fatalf("network error should NOT be client error (retry)")
	}
}

func TestConsume_NetworkError(t *testing.T) {
	// Point at a closed server to trigger a dial error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	cli := &OpenAIClient{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := cli.Consume(CodexCredentials{AccessToken: "t"}, "rrid", "cid")
	if err == nil {
		t.Fatalf("expected network error")
	}
}

func TestFullURLEqualsDefaultBasePlusPath(t *testing.T) {
	// Locks the documented full-URL constants (constants.go) against the
	// production BaseURL + path constants used by the client.
	if defaultBaseURL+resetCreditsEndpointPath != resetCreditsEndpoint {
		t.Fatalf("reset-credits URL mismatch: %q + %q != %q", defaultBaseURL, resetCreditsEndpointPath, resetCreditsEndpoint)
	}
	if defaultBaseURL+consumeEndpointPath != consumeEndpoint {
		t.Fatalf("consume URL mismatch")
	}
	if defaultBaseURL+usageEndpointPath != usageEndpoint {
		t.Fatalf("usage URL mismatch")
	}
}
