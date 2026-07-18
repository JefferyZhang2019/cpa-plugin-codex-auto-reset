package main

import (
	"errors"
	"testing"
)

func TestBackoffForAttempt(t *testing.T) {
	want := []int{1, 2, 5, 10, 30}
	for i, minutes := range want {
		got := backoffForAttempt(i + 1)
		if int(got.Minutes()) != minutes {
			t.Fatalf("attempt %d => %v, want %dm", i+1, got, minutes)
		}
	}
	// Out of range clamps to last value.
	if got := backoffForAttempt(99); got != resetRetryDelays[len(resetRetryDelays)-1] {
		t.Fatalf("attempt 99 => %v", got)
	}
	// Zero/negative also clamp to last (defensive).
	if got := backoffForAttempt(0); got != resetRetryDelays[len(resetRetryDelays)-1] {
		t.Fatalf("attempt 0 => %v", got)
	}
}

func TestClassifyConsume_SuccessCodes(t *testing.T) {
	cases := []struct {
		name string
		resp ConsumeResponse
		want retryDecision
	}{
		{"reset", ConsumeResponse{Code: ConsumeCodeReset}, decisionSuccess},
		{"already_redeemed", ConsumeResponse{Code: ConsumeCodeAlreadyRedeemed}, decisionSuccess},
		{"no_credit", ConsumeResponse{Code: ConsumeCodeNoCredit}, decisionStop},
		{"nothing_to_reset", ConsumeResponse{Code: ConsumeCodeNothingToReset}, decisionStop},
		{"unknown_code", ConsumeResponse{Code: "garbage"}, decisionStop},
		{"empty_code", ConsumeResponse{}, decisionStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyConsume(tc.resp, nil); got != tc.want {
				t.Fatalf("classify %s => %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// sentinel error types for the retry classification tests.
type netErr struct{}

func (netErr) Error() string { return "dial tcp: connection refused" }

func TestClassifyConsume_ErrorClassification(t *testing.T) {
	// 4xx -> hard stop (do NOT retry a known-bad token for 48 minutes).
	// 5xx and network errors -> retry.
	// Plain (non-HTTPStatusError) errors are treated as retryable (network,
	// timeout, DNS, etc.).
	cases := []struct {
		name string
		err  error
		want retryDecision
	}{
		{"4xx_client_error", &HTTPStatusError{Code: 401}, decisionStop},
		{"4xx_forbidden", &HTTPStatusError{Code: 403}, decisionStop},
		{"5xx_server_error", &HTTPStatusError{Code: 502}, decisionRetry},
		{"5xx_gateway_timeout", &HTTPStatusError{Code: 504}, decisionRetry},
		{"plain_network_error", netErr{}, decisionRetry},
		{"wrapped_network_error", errors.New("wrapped: "+(netErr{}).Error()), decisionRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyConsume(ConsumeResponse{}, tc.err); got != tc.want {
				t.Fatalf("classify err %s => %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
