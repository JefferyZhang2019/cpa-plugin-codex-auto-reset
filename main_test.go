package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPluginRegistration(t *testing.T) {
	reg := pluginRegistration()
	if reg.Metadata.Name != "Codex Auto Reset" {
		t.Fatalf("name = %q", reg.Metadata.Name)
	}
	if reg.Capabilities.ManagementAPI != true {
		t.Fatalf("management API not enabled")
	}
	if reg.SchemaVersion == 0 {
		t.Fatalf("schema version unset")
	}
}

func TestManagementRegistrationResponse_HasExpectedRoutes(t *testing.T) {
	resp := managementRegistrationResponse()
	if len(resp.Routes) < 8 {
		t.Fatalf("expected >=8 routes, got %d", len(resp.Routes))
	}
	if len(resp.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resp.Resources))
	}
	if resp.Resources[0].Path != "/status" {
		t.Fatalf("resource path = %q", resp.Resources[0].Path)
	}
}

func TestHandleMethod_UnknownReturnsError(t *testing.T) {
	// configurePlugin requires host callbacks; skip wiring and just check unknown method.
	raw, err := handleMethod("totally.unknown", nil)
	if err != nil {
		t.Fatalf("unknown method should not return Go error: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("expected envelope body")
	}
	// Body should contain the error code.
	body := string(raw)
	if !contains(body, "unknown_method") {
		t.Fatalf("body missing error code: %s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestBackfillResetHistory_NoDuplicateForSameReset verifies the backfill does
// not append a second record when the SAME reset already exists in history
// with a nanosecond-different timestamp (history from pendingHistory uses one
// time.Now() reading; the log entry uses another).
func TestBackfillResetHistory_NoDuplicateForSameReset(t *testing.T) {
	authID := "acct-1"
	ts1 := time.Date(2026, 9, 20, 1, 6, 22, 123456789, time.UTC)  // history record
	ts2 := time.Date(2026, 9, 20, 1, 6, 22, 987654321, time.UTC)  // log entry (same second, different ns)
	state := &PluginState{
		ResetHistory: []ResetRecord{{
			Timestamp: ts1, AuthID: authID, CreditID: "credit-A", Success: true,
			PreWeeklyPct: 0, PostWeeklyPct: 100, PreCredits: 3, PostCredits: 2,
		}},
		Logs: []LogEntry{{
			Timestamp: ts2, Level: "info", Scope: authID, State: StateDONE,
			Message: "reset verified OK: credits 3→2, weekly 0%→100%, target credit-A consumed",
			Details: map[string]any{
				"credit_id": "credit-A", "pre_credits": 3.0, "post_credits": 2.0,
				"pre_weekly_pct": 0.0, "post_weekly_pct": 100.0,
			},
		}},
	}
	backfillResetHistoryFromLogs(state)
	if len(state.ResetHistory) != 1 {
		t.Fatalf("expected 1 record after backfill, got %d (duplicate appended)", len(state.ResetHistory))
	}
}

// TestBackfillResetHistory_CollapsesExistingDuplicates verifies records that
// were duplicated by the old nanosecond-dedup bug get collapsed on load.
func TestBackfillResetHistory_CollapsesExistingDuplicates(t *testing.T) {
	base := time.Date(2026, 9, 20, 1, 6, 22, 0, time.UTC)
	state := &PluginState{
		ResetHistory: []ResetRecord{
			{Timestamp: base.Add(100 * time.Nanosecond), AuthID: "a", CreditID: "c1", Success: true, PostWeeklyPct: 100},
			{Timestamp: base.Add(900 * time.Nanosecond), AuthID: "a", CreditID: "c1", Success: true, PostWeeklyPct: 100},
		},
	}
	backfillResetHistoryFromLogs(state)
	if len(state.ResetHistory) != 1 {
		t.Fatalf("expected duplicates collapsed to 1, got %d", len(state.ResetHistory))
	}
}

// TestBackfillResetHistory_StillBackfillsOldLogs verifies genuine old resets
// (history empty, logs present) are still backfilled exactly once.
func TestBackfillResetHistory_StillBackfillsOldLogs(t *testing.T) {
	ts := time.Date(2026, 7, 31, 22, 16, 35, 0, time.UTC)
	state := &PluginState{
		Logs: []LogEntry{{
			Timestamp: ts, Level: "info", Scope: "old-acct", State: StateDONE,
			Message: "reset verified OK: credits 2→1, weekly 91%→100%, target x consumed",
			Details: map[string]any{
				"credit_id": "x", "pre_credits": 2.0, "post_credits": 1.0,
				"pre_weekly_pct": 91.0, "post_weekly_pct": 100.0,
			},
		}},
	}
	backfillResetHistoryFromLogs(state)
	if len(state.ResetHistory) != 1 || !state.ResetHistory[0].Success {
		t.Fatalf("expected 1 backfilled success, got %+v", state.ResetHistory)
	}
}

// TestResolveStatePath_Precedence verifies the durability ladder: explicit
// absolute state_dir wins, then the auth-dir-derived location (what makes
// state survive container recreates), then the legacy user-config default.
// Relative/empty overrides and unusable auth paths must fall through.
// Paths are built platform-neutrally (tests run on Windows and Linux).
func TestResolveStatePath_Precedence(t *testing.T) {
	tmp := t.TempDir()
	explicit := filepath.Join(tmp, "persist")
	authDir := filepath.Join(tmp, "auths")
	entries := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "runtime-only"}, // no Path — skipped
		{AuthIndex: "codex-1", Path: filepath.Join(authDir, "codex-1.json")},
	}
	derived := filepath.Join(authDir, "plugin-state", "codex-auto-reset", "state.json")
	cases := []struct {
		name     string
		stateDir string
		entries  []pluginapi.HostAuthFileEntry
		want     string
	}{
		{"explicit wins", explicit, entries, filepath.Join(explicit, "state.json")},
		{"explicit trimmed", "  " + explicit + "  ", entries, filepath.Join(explicit, "state.json")},
		{"relative override ignored", "relative", entries, derived},
		{"auth dir derived", "", entries, derived},
		{"relative auth path ignored", "", []pluginapi.HostAuthFileEntry{{Path: "auths/codex-1.json"}}, defaultStatePath()},
		{"no auth entries falls back", "", nil, defaultStatePath()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStatePath(tc.stateDir, tc.entries)
			if got != tc.want {
				t.Fatalf("resolveStatePath(%q, ...) = %q, want %q", tc.stateDir, got, tc.want)
			}
		})
	}
}

// TestResolveStatePath_PlacesStateUnderAuthDir documents the container case
// that motivated the auth-dir anchor: state lands under the auth dir (the
// tree operators volume-mount), not the ephemeral user-config dir.
func TestResolveStatePath_PlacesStateUnderAuthDir(t *testing.T) {
	authDir := t.TempDir()
	entries := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "a", Path: filepath.Join(authDir, "codex-team.json")},
	}
	got := resolveStatePath("", entries)
	want := filepath.Join(authDir, "plugin-state", "codex-auto-reset", "state.json")
	if got != want {
		t.Fatalf("state not under auth dir: got %q want %q", got, want)
	}
}
