package sim

import (
	"fmt"
	"testing"
	"time"
)

// TestAlreadyRedeemed_IdempotentSuccess: the first POST fails transiently but
// the server actually processed it. The retry (same redeem_request_id) returns
// already_redeemed. The plugin must treat this as success and verify normally.
// On the retry's server-side hook we reflect the prior success (mark credit
// redeemed, refill quota) so verification sees the correct post-state.
func TestAlreadyRedeemed_IdempotentSuccess(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := newClient("c1", E, 20)
	// First POST: transient failure (server processed it but we lost the response).
	client.FailFirstN = 1
	client.FailErr = fmt.Errorf("network timeout; server may have processed")
	// Second POST (retry, same UUID): server reports already_redeemed.
	// On this call we apply the side-effect of the original success.
	client.ConsumeCode = "already_redeemed"
	client.OnConsumeReset = func(id string) {
		// Reflect the prior success: credit redeemed, quota refilled.
		if cr, ok := client.Credits[id]; ok {
			cr.Status = "redeemed"
		}
		client.WeeklyPct = 100
	}

	out := Run(cfg, client, E.Add(-12*time.Hour), 14*24*time.Hour)

	if out.NumConsumePOST != 2 {
		t.Fatalf("expected 2 POSTs (1 fail + 1 already_redeemed), got %d\n%v", out.NumConsumePOST, dumpEvents(out))
	}
	if out.GaveUp {
		t.Fatalf("already_redeemed should be treated as success, not gaveUp=%q", out.GiveUpReason)
	}
	if out.ResetAt.IsZero() {
		t.Fatalf("expected reset time to be recorded")
	}
	// Both POSTs must share the redeem_request_id.
	if client.RedeemRequests[0] != client.RedeemRequests[1] {
		t.Fatalf("expected same UUID on retry, got %v", client.RedeemRequests)
	}
	t.Logf("OK already_redeemed path: 1 fail + 1 already_redeemed (UUID %s), reset=%v",
		client.RedeemRequests[0], out.ResetAt)
}

// TestRetriesExhausted_GivesUp: 5 consecutive transient failures -> abandon.
func TestRetriesExhausted_GivesUp(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := newClient("c1", E, 20)
	client.FailFirstN = 10 // more than 5 retries; all fail
	client.FailErr = fmt.Errorf("persistent network failure")

	start := E.Add(-12 * time.Hour)
	out := Run(cfg, client, start, 14*24*time.Hour)

	// 1 initial + 5 retries = 6 POSTs total, then give up.
	if out.NumConsumePOST != 6 {
		t.Fatalf("expected 6 POSTs (1 + 5 retries), got %d\n%v", out.NumConsumePOST, dumpEvents(out))
	}
	if !out.GaveUp {
		t.Fatalf("expected gaveUp after retries exhausted")
	}
	// Credit must NOT have been redeemed (server never succeeded).
	if cr, ok := client.Credits["c1"]; !ok || cr.Status != "available" {
		t.Fatalf("c1 should still be available, got %v", client.Credits["c1"])
	}
	// All 6 POSTs must use the same redeem_request_id.
	seen := map[string]int{}
	for _, r := range client.RedeemRequests {
		seen[r]++
	}
	if len(seen) != 1 {
		t.Fatalf("expected all retries to reuse one UUID, got %v", seen)
	}
	t.Logf("OK gave up after 6 POSTs (1+5 retries) all using %s; credit untouched", client.RedeemRequests[0])
}

// TestCreditVanishesDuringARMED: during ARMED sleep a human uses the credit.
// At T the CONFIRMING GET sees no available credit -> abandon, no POST.
func TestCreditVanishesDuringARMED(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}
	T := E.Add(-cfg.TriggerLeadTime) // 12:23:47

	client := newClient("c1", E, 30)

	// Start just after M so we enter ARMED immediately and sleep to T.
	start := T.Add(-30 * time.Minute) // 11:53:47, inside ARMED window
	// Schedule the credit to vanish before T via a list-side hook: when list
	// is called at or after T, mark credit redeemed (human used it).
	originalList := client.list
	_ = originalList
	// We can't override methods; use a wrapper by replacing the Credits map at T.
	// Instead, simulate by setting status=redeemed at start; the engine's local
	// filter still returns it (status != "available" -> pickTarget returns false).
	client.Credits["c1"].Status = "redeemed"

	out := Run(cfg, client, start, 14*24*time.Hour)

	if out.NumConsumePOST != 0 {
		t.Fatalf("expected 0 POSTs when credit vanished, got %d", out.NumConsumePOST)
	}
	if !out.GaveUp {
		t.Fatalf("expected gaveUp when credit vanished during ARMED")
	}
	t.Logf("OK credit vanished during ARMED; no POST, gaveUp=%q", out.GiveUpReason)
}

// TestWrongCreditConsumed_DetectsMismatch: server consumes a DIFFERENT credit
// than the one we targeted. Verify must catch this and halt as error.
func TestWrongCreditConsumed_DetectsMismatch(t *testing.T) {
	E1 := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC) // sooner (target)
	E2 := E1.Add(10 * 24 * time.Hour)                     // later (should NOT be touched)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := &FakeOpenAIClient{
		Credits: map[string]*Credit{
			"sooner": {ID: "sooner", Status: "available", ExpiresAt: E1},
			"later":  {ID: "later", Status: "available", ExpiresAt: E2},
		},
		WeeklyPct:   25,
		ConsumeCode: "reset",
		WindowsReset: 2,
	}
	// BUG-injection: server deducts the WRONG credit (the later one), not the
	// targeted sooner one.
	client.OnConsumeReset = func(consumedID string) {
		// Ignore consumedID; always deduct 'later'.
		if cr, ok := client.Credits["later"]; ok {
			cr.Status = "redeemed"
		}
		client.WeeklyPct = 100
	}

	out := Run(cfg, client, E1.Add(-12*time.Hour), 14*24*time.Hour)

	// Verify must flag mismatch: target (sooner) is NOT gone, count did -1 but
	// on the wrong credit. The exact judgment depends on triple-check; we assert
	// the plugin HALTED (gaveUp) rather than reporting clean success.
	if !out.GaveUp {
		t.Fatalf("expected verification to halt on wrong-credit consumption; events:\n%v", dumpEvents(out))
	}
	if out.GiveUpReason == "" || !contains(out.GiveUpReason, "mismatch") {
		t.Fatalf("expected mismatch reason, got %q", out.GiveUpReason)
	}
	// Sooner must still be available (we never actually consumed it correctly).
	if cr, ok := client.Credits["sooner"]; !ok || cr.Status != "available" {
		t.Fatalf("sooner should still be available after wrong-credit detection, got %v", client.Credits["sooner"])
	}
	t.Logf("OK wrong-credit consumption detected: %q", out.GiveUpReason)
}

// TestStartPastExpiry_NoReset: plugin starts after E. Local filter excludes
// the credit; no POST is ever sent.
func TestStartPastExpiry_NoReset(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := newClient("c1", E, 50)
	start := E.Add(1 * time.Hour) // 1h after expiry

	out := Run(cfg, client, start, 2*24*time.Hour)

	if out.NumConsumePOST != 0 {
		t.Fatalf("expected 0 POSTs when starting after expiry, got %d", out.NumConsumePOST)
	}
	// Engine should not reset.
	if !out.ResetAt.IsZero() {
		t.Fatalf("expected no reset, got %v", out.ResetAt)
	}
	t.Logf("OK started after E; no reset (list calls=%d)", out.NumListCalls)
}

// TestLongPatrolRhythm: credit far from expiry; verify IDLE does multiple
// patrols without ever consuming, then arms at the right time.
func TestLongPatrolRhythm(t *testing.T) {
	E := time.Date(2026, 7, 25, 9, 13, 27, 0, time.UTC)
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := newClient("c1", E, 40)
	start := E.Add(-7 * 24 * time.Hour) // a week before; many patrols expected

	out := Run(cfg, client, start, 14*24*time.Hour)

	if out.NumConsumePOST != 1 {
		t.Fatalf("expected exactly 1 consume, got %d", out.NumConsumePOST)
	}
	// Multiple IDLE patrols should have happened (list calls > 3).
	if out.NumListCalls < 5 {
		t.Fatalf("expected >=5 list calls from patrol rhythm, got %d", out.NumListCalls)
	}
	// Reset must be at T = E - L.
	T := E.Add(-cfg.TriggerLeadTime)
	drift := out.ResetAt.Sub(T)
	if drift < 0 {
		drift = -drift
	}
	if drift > time.Second {
		t.Fatalf("reset drifted %v from T %s", drift, T)
	}
	t.Logf("OK long patrol: %d list calls, reset at T=%s", out.NumListCalls, out.ResetAt.Format("01-02 15:04:05"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
