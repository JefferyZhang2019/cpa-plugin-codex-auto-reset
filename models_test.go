package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCreditSortOrder(t *testing.T) {
	t1 := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	t2 := t1.Add(48 * time.Hour)
	in := []Credit{
		{ID: "later", Status: "available", ExpiresAt: t2},
		{ID: "never", Status: "available", ExpiresAt: time.Time{}}, // zero = never expires
		{ID: "sooner", Status: "available", ExpiresAt: t1},
		{ID: "used", Status: "redeemed", ExpiresAt: t1.Add(-time.Hour)},
	}
	got := availableCreditsSorted(in, t1.Add(-time.Hour))
	if len(got) != 3 {
		t.Fatalf("got %d available, want 3: %+v", len(got), got)
	}
	if got[0].ID != "sooner" || got[1].ID != "later" || got[2].ID != "never" {
		t.Fatalf("order = %s %s %s; never-expiry must sort last", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestCreditSort_FiltersExpiredLocally(t *testing.T) {
	// Even if status says "available", an expires_at in the past must be filtered.
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	in := []Credit{
		{ID: "expired", Status: "available", ExpiresAt: now.Add(-time.Hour)},
		{ID: "fresh", Status: "available", ExpiresAt: now.Add(time.Hour)},
	}
	got := availableCreditsSorted(in, now)
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("expected only 'fresh', got %+v", got)
	}
}

func TestLogEntryJSONRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	entry := LogEntry{
		Timestamp: ts, Level: "info", Scope: "acct1", State: StateARMED,
		Message: "armed", NextAction: "sleep to T",
		NextAt: &ts, Details: map[string]any{"credit_id": "c1"},
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back LogEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Scope != "acct1" || back.State != StateARMED || back.Details["credit_id"] != "c1" {
		t.Fatalf("round-trip lost data: %+v", back)
	}
}

func TestStateStringConstants(t *testing.T) {
	// Names are a stable contract — later tasks switch on them.
	cases := map[State]string{
		StateIDLE: "IDLE", StateARMED: "ARMED", StateCONFIRMING: "CONFIRMING",
		StateRESETTING: "RESETTING", StateVERIFYING: "VERIFYING", StateDONE: "DONE",
	}
	for st, want := range cases {
		if string(st) != want {
			t.Fatalf("state %q != %q", st, want)
		}
	}
}
