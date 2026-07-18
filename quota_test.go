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
	raw := []byte(`{
	  "plan_type":"plus",
	  "rate_limit_reset_credits":{"available_count":1},
	  "rate_limits":[
	    {"kind":"weekly","window_seconds":604800,"used_percent":15,"reset_at":"2026-07-25T00:00:00Z"}
	  ]
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// weekly used 15% => remaining 85
	if snap.WeeklyPct != 85 {
		t.Fatalf("WeeklyPct = %d, want 85", snap.WeeklyPct)
	}
	if snap.AvailableCount != 1 {
		t.Fatalf("available_count = %d", snap.AvailableCount)
	}
}

func TestParseUsageResponse_NoWeeklyWindowDefaultsFull(t *testing.T) {
	// No weekly window in rate_limits -> treat as full (100%).
	raw := []byte(`{"plan_type":"plus","rate_limits":[]}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.WeeklyPct != 100 {
		t.Fatalf("WeeklyPct = %d, want 100 when no weekly window", snap.WeeklyPct)
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
