package main

import (
	"testing"
	"time"
)

func TestParseResetCreditsResponse_UpstreamContractFixture(t *testing.T) {
	raw := []byte(`{
	  "credits": [
	    {"id":"credit-1","reset_type":"codex_rate_limits","status":"available",
	     "granted_at":"2026-06-17T00:00:00Z","expires_at":"2026-07-17T00:00:00Z",
	     "title":"Full reset","description":"Ready"},
	    {"id":"credit-2","reset_type":"codex_rate_limits","status":"available",
	     "granted_at":"2026-06-18T00:00:00Z","expires_at":null}
	  ],
	  "available_count": 2,
	  "total_earned_count": 4
	}`)
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	snap, err := parseResetCreditsResponse(raw, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.AvailableCount != 2 {
		t.Fatalf("available_count = %d", snap.AvailableCount)
	}
	if len(snap.Credits) != 2 {
		t.Fatalf("credits len = %d", len(snap.Credits))
	}
	// credit-2 has null expires_at — must be present and zero-valued (never expires).
	if snap.Credits[1].ID != "credit-2" || !snap.Credits[1].ExpiresAt.IsZero() {
		t.Fatalf("credit-2 parse wrong: %+v", snap.Credits[1])
	}
}

func TestParseUsageResponse_WeeklyPercent(t *testing.T) {
	// Real /wham/usage shape (verified against codex-quota-scheduler): the
	// weekly window is code_review_rate_limit.secondary_window, not a flat
	// rate_limits[] array. used_percent 15 -> remaining 85.
	raw := []byte(`{
	  "plan_type":"plus",
	  "rate_limit_reset_credits":{"available_count":1},
	  "code_review_rate_limit":{
	    "primary_window":{"used_percent":42,"limit_window_seconds":18000},
	    "secondary_window":{"used_percent":15,"limit_window_seconds":604800}
	  }
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// weekly (secondary) used 15% => remaining 85
	if snap.WeeklyPct != 85 {
		t.Fatalf("WeeklyPct = %d, want 85", snap.WeeklyPct)
	}
	if snap.AvailableCount != 1 {
		t.Fatalf("available_count = %d", snap.AvailableCount)
	}
}

func TestParseUsageResponse_NoSecondaryWindowDefaultsFull(t *testing.T) {
	// No secondary_window (e.g. freshly-reset account) -> treat as full (100%).
	// Must NOT misread primary_window's used_percent as the weekly value.
	raw := []byte(`{
	  "plan_type":"plus",
	  "code_review_rate_limit":{
	    "primary_window":{"used_percent":99,"limit_window_seconds":18000}
	  }
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.WeeklyPct != 100 {
		t.Fatalf("WeeklyPct = %d, want 100 when no secondary window (got primary bleed?)", snap.WeeklyPct)
	}
}

func TestParseUsageResponse_NoCodeReviewRateLimitDefaultsFull(t *testing.T) {
	raw := []byte(`{"plan_type":"plus"}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.WeeklyPct != 100 {
		t.Fatalf("WeeklyPct = %d, want 100 when no rate-limit block", snap.WeeklyPct)
	}
}

func TestParseUsageResponse_OverageClampsToZero(t *testing.T) {
	// used_percent > 100 (server-side overage) must clamp remaining to 0, not go negative.
	raw := []byte(`{
	  "code_review_rate_limit":{
	    "secondary_window":{"used_percent":150,"limit_window_seconds":604800}
	  }
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.WeeklyPct != 0 {
		t.Fatalf("WeeklyPct = %d, want 0 (clamped)", snap.WeeklyPct)
	}
}

func TestParseUsageResponse_TopLevelRateLimitPath(t *testing.T) {
	// Some CPA builds put the rate-limit windows under the top-level
	// "rate_limit" key (not "code_review_rate_limit"). The parser must find
	// the weekly % from either path.
	raw := []byte(`{
	  "plan_type":"plus",
	  "rate_limit_reset_credits":{"available_count":2},
	  "rate_limit":{
	    "primary_window":{"used_percent":50,"limit_window_seconds":18000},
	    "secondary_window":{"used_percent":81,"limit_window_seconds":604800}
	  }
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// used 81% -> remaining 19%
	if snap.WeeklyPct != 19 {
		t.Fatalf("WeeklyPct = %d, want 19", snap.WeeklyPct)
	}
	if snap.AvailableCount != 2 {
		t.Fatalf("available_count = %d", snap.AvailableCount)
	}
}

func TestParseUsageResponse_CamelCaseFallback(t *testing.T) {
	// camelCase variant of field names.
	raw := []byte(`{
	  "rateLimit":{
	    "secondaryWindow":{"usedPercent":30}
	  }
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.WeeklyPct != 70 {
		t.Fatalf("WeeklyPct = %d, want 70", snap.WeeklyPct)
	}
}

func TestParseMalformedJSON_AllParsersReturnError(t *testing.T) {
	garbage := []byte(`{not valid json`)
	if _, err := parseResetCreditsResponse(garbage, time.Now()); err == nil {
		t.Fatalf("parseResetCreditsResponse: expected error on malformed JSON")
	}
	if _, err := parseUsageResponse(garbage, time.Now()); err == nil {
		t.Fatalf("parseUsageResponse: expected error on malformed JSON")
	}
	if _, err := parseConsumeResponse(garbage); err == nil {
		t.Fatalf("parseConsumeResponse: expected error on malformed JSON")
	}
}

func TestParseConsumeResponse_AllCodes(t *testing.T) {
	cases := []struct {
		code string
		want ConsumeCode
	}{
		{`"reset"`, ConsumeCodeReset},
		{`"already_redeemed"`, ConsumeCodeAlreadyRedeemed},
		{`"no_credit"`, ConsumeCodeNoCredit},
		{`"nothing_to_reset"`, ConsumeCodeNothingToReset},
	}
	for _, tc := range cases {
		raw := []byte(`{"code":` + tc.code + `,"windows_reset":2}`)
		resp, err := parseConsumeResponse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.code, err)
		}
		if resp.Code != tc.want {
			t.Fatalf("code %s => %v, want %v", tc.code, resp.Code, tc.want)
		}
		if resp.WindowsReset != 2 {
			t.Fatalf("windows_reset = %d", resp.WindowsReset)
		}
	}
}

func TestParseConsumeResponse_DefaultsWindowsReset(t *testing.T) {
	// windows_reset is omitempty in upstream; absent must default to 0.
	raw := []byte(`{"code":"reset"}`)
	resp, err := parseConsumeResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.WindowsReset != 0 {
		t.Fatalf("windows_reset = %d, want 0", resp.WindowsReset)
	}
}
