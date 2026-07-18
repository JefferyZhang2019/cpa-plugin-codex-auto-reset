package sim

import (
	"fmt"
	"testing"
	"time"
)

// newClient builds a fake client with one available credit expiring at E and a
// server-side consume hook that marks the credit redeemed and refills weekly quota.
func newClient(creditID string, expiresAt time.Time, preWeekly int) *FakeOpenAIClient {
	c := &FakeOpenAIClient{
		Credits: map[string]*Credit{
			creditID: {ID: creditID, Status: "available", ExpiresAt: expiresAt},
		},
		WeeklyPct:   preWeekly,
		ConsumeCode: "reset",
		WindowsReset: 2,
	}
	c.OnConsumeReset = func(consumedID string) {
		if cr, ok := c.Credits[consumedID]; ok {
			cr.Status = "redeemed"
		}
		c.WeeklyPct = 100
	}
	return c
}

// TestMatrix_AllRelations covers R==L, R>L, R<L at non-round timestamps.
// For each (R, L) pair we run the same battery of start phases.
func TestMatrix_AllRelations(t *testing.T) {
	// E is identical across cases; only R and L vary. Non-round to expose rounding bugs.
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)

	relations := []struct {
		name string
		R    time.Duration
		L    time.Duration
	}{
		// Integer-multiple relations (original matrix).
		{"R_eq_L_6_6", 6 * time.Hour, 6 * time.Hour},
		{"R_eq_L_12_12", 12 * time.Hour, 12 * time.Hour},
		{"R_gt_L_12_6", 12 * time.Hour, 6 * time.Hour},
		{"R_gt_L_24_12", 24 * time.Hour, 12 * time.Hour},
		{"R_lt_L_6_12", 6 * time.Hour, 12 * time.Hour},
		{"R_lt_L_12_24", 12 * time.Hour, 24 * time.Hour},
		{"R_lt_L_6_24", 6 * time.Hour, 24 * time.Hour},
		// Non-integer-multiple relations — these stress IDLE patrol phase
		// alignment against M and T, where patrol landings do not coincide
		// with neat multiples of R.
		{"R_gt_L_5_3", 5 * time.Hour, 3 * time.Hour},              // R>L, coprime hours
		{"R_gt_L_7_5", 7 * time.Hour, 5 * time.Hour},              // R>L, coprime
		{"R_gt_L_8_6", 8 * time.Hour, 6 * time.Hour},              // R>L, non-multiple
		{"R_gt_L_10_3", 10 * time.Hour, 3 * time.Hour},            // R>>L
		{"R_lt_L_3_5", 3 * time.Hour, 5 * time.Hour},              // R<L, coprime
		{"R_lt_L_5_7", 5 * time.Hour, 7 * time.Hour},              // R<L, coprime
		{"R_lt_L_3_10", 3 * time.Hour, 10 * time.Hour},            // R<<L
		{"R_lt_L_7_11", 7 * time.Hour, 11 * time.Hour},            // R<L, coprime primes
		// Sub-hour granularity to catch minute/second rounding.
		{"R_gt_L_90min_45min", 90 * time.Minute, 45 * time.Minute},
		{"R_lt_L_37min_61min", 37 * time.Minute, 61 * time.Minute},
	}

	// Start phases, each non-round. Comment explains the case.
	phases := []struct {
		name   string
		offset time.Duration // relative to E; negative = before E
	}{
		{"far_before_M", -48 * time.Hour}, // well before M; multiple IDLE cycles
		{"just_before_M_7s", 0},            // filled in per-case below (needs M)
		{"just_after_M_7s", 0},             // filled in per-case below (needs M)
		{"inside_ARMED_window", 0},         // M < now < T
		{"just_before_T_3min", 0},          // 3 min before T
		{"at_T_exact", 0},                  // exactly T
		{"past_T_before_E_2h", 0},          // 2h past T, still before E
		{"past_E_5min", 0},                 // 5 min after expiry (credit should be filtered)
		// Non-multiple stress phases: simulate IDLE patrol rhythms that don't
		// align with M or T. These are filled in per-case below.
		{"patrol_lands_13s_before_M", 0}, // a patrol wake 13s before M -> next wake is after M, arms
		{"patrol_straddles_M", 0},        // patrol wake exactly at M - R/2, next at M + R/2
		{"two_patrols_then_arm", 0},      // start 2R before M, two patrols, third lands just past M
		{"long_drift_far_before", 0},     // start 5R+17min before M to exercise drift
	}

	for _, rel := range relations {
		rel := rel
		T := E.Add(-rel.L)
		M := T.Add(-rel.R)

		// Resolve the per-case phases that depend on M and T.
		resolved := make([]struct {
			name   string
			offset time.Duration
		}, len(phases))
		for i, p := range phases {
			resolved[i].name = p.name
			switch p.name {
			case "just_before_M_7s":
				resolved[i].offset = M.Add(-7 * time.Second).Sub(E)
			case "just_after_M_7s":
				resolved[i].offset = M.Add(7 * time.Second).Sub(E)
			case "inside_ARMED_window":
				// midpoint between M and T (or just after M if R is small)
				mid := M.Add(T.Sub(M) / 2)
				resolved[i].offset = mid.Sub(E)
			case "just_before_T_3min":
				resolved[i].offset = T.Add(-3 * time.Minute).Sub(E)
			case "at_T_exact":
				resolved[i].offset = T.Sub(E)
			case "past_T_before_E_2h":
				// 2h past T if that stays before E; else midpoint between T and E
				cand := T.Add(2 * time.Hour)
				if cand.After(E) {
					cand = T.Add(E.Sub(T) / 2)
				}
				resolved[i].offset = cand.Sub(E)
			case "past_E_5min":
				resolved[i].offset = E.Add(5 * time.Minute).Sub(E) // = 5min, positive
			case "patrol_lands_13s_before_M":
				// A patrol wake 13s before M; next patrol (M + R - 13s) lands well past M.
				resolved[i].offset = M.Add(-13 * time.Second).Sub(E)
			case "patrol_straddles_M":
				// Patrol wake exactly at M - R/2; next wake at M + R/2 (past M).
				resolved[i].offset = M.Add(-rel.R / 2).Sub(E)
			case "two_patrols_then_arm":
				// Start 2R before M; patrols at -2R, -R, 0(=M) -> arms on third.
				resolved[i].offset = M.Add(-2 * rel.R).Sub(E)
			case "long_drift_far_before":
				// Start 5R + 17min before M; many patrols with a 17min phase offset.
				resolved[i].offset = M.Add(-5*rel.R - 17*time.Minute).Sub(E)
			default:
				resolved[i].offset = p.offset
			}
		}

		t.Run(rel.name, func(t *testing.T) {
			for _, ph := range resolved {
				ph := ph
				startNow := E.Add(ph.offset)
				t.Run(ph.name, func(t *testing.T) {
					runOneCase(t, rel.R, rel.L, E, T, M, startNow, ph.name)
				})
			}
		})
	}
}

func runOneCase(t *testing.T, R, L time.Duration, E, T, M, startNow time.Time, phaseName string) {
	t.Helper()
	cfg := Config{RefreshInterval: R, TriggerLeadTime: L, PostResetDelay: 1 * time.Minute}
	client := newClient("c1", E, 15) // pre-reset weekly at 15%
	// Sim budget: must cover the worst-case path from startNow to T (the reset
	// target) plus verify delay. The longest path is the "long_drift_far_before"
	// phase starting ~5R before M, so we need up to ~6R + L to reach T from
	// there, plus a buffer. Use an absolute floor of 48h.
	need := T.Sub(startNow)
	if need < 0 {
		need = 0
	}
	maxSim := need + L + 2*time.Hour // L gets us from T to E (headroom); +2h buffer
	if maxSim < 48*time.Hour {
		maxSim = 48 * time.Hour
	}

	out := Run(cfg, client, startNow, maxSim)

	// Past-expiry phase: credit should have been locally filtered, no reset.
	if startNow.After(E) || startNow.Equal(E) {
		if out.NumConsumePOST != 0 {
			t.Fatalf("phase=%s: started at/after E but still POSTed consume %d times", phaseName, out.NumConsumePOST)
		}
		if out.ResetAt.IsZero() {
			// Expected: no reset because credit is expired. This is success.
			t.Logf("OK phase=%s: credit expired at start, no reset (gaveUp=%v reason=%q)", phaseName, out.GaveUp, out.GiveUpReason)
			return
		}
		t.Fatalf("phase=%s: started after E but somehow reset", phaseName)
	}

	// For all pre-expiry phases, we MUST have reset exactly once.
	if out.NumConsumePOST != 1 {
		t.Fatalf("phase=%s: expected exactly 1 POST /consume, got %d (events:\n%v)", phaseName, out.NumConsumePOST, dumpEvents(out))
	}
	if out.ResetAt.IsZero() {
		t.Fatalf("phase=%s: no reset recorded (events:\n%v)", phaseName, dumpEvents(out))
	}

	// Reset must happen before E (the whole point).
	if !out.ResetAt.Before(E) {
		t.Fatalf("phase=%s: reset at %s is not before E %s", phaseName, out.ResetAt, E)
	}

	// Reset must happen at or after T (we never reset before the window opens).
	if out.ResetAt.Before(T) {
		t.Fatalf("phase=%s: reset at %s is before T %s (reset before window opened!)", phaseName, out.ResetAt, T)
	}

	// Distance from reset to E must be <= L (we never let it expire).
	dist := E.Sub(out.ResetAt)
	if dist > L {
		t.Fatalf("phase=%s: reset %s is > L (%v) before E; would risk expiry", phaseName, out.ResetAt, L)
	}

	// If the plugin started at or after T, reset happens "now-ish" (>= startNow),
	// distance to E will be < L. That's expected. If started before T, reset should
	// be very close to T (within the post-reset delay plus epsilon).
	if startNow.Before(T) {
		// Reset should land essentially at T (we sleep to T then confirm immediately).
		drift := out.ResetAt.Sub(T)
		if drift < 0 {
			drift = -drift
		}
		if drift > time.Second {
			t.Fatalf("phase=%s: reset at %s drifted %v from T %s", phaseName, out.ResetAt, drift, T)
		}
	}

	// Verification must have happened: list + usage calls after the POST.
	if out.NumUsageCalls == 0 {
		t.Fatalf("phase=%s: no usage call recorded during verification", phaseName)
	}

	// Final state must be DONE.
	if out.FinalState != StateDONE {
		t.Fatalf("phase=%s: final state=%s want DONE", phaseName, out.FinalState)
	}

	t.Logf("OK phase=%s: reset at %s (dist-to-E=%v, L=%v) consumes=%d list=%d usage=%d",
		phaseName, out.ResetAt.Format("01-02 15:04:05"), dist.Round(time.Second), L,
		out.NumConsumePOST, out.NumListCalls, out.NumUsageCalls)
}

func dumpEvents(o *Outcome) string {
	if o == nil {
		return "<nil>"
	}
	out := ""
	for _, ev := range o.Events {
		next := ""
		if ev.Next != nil {
			next = " next=" + ev.Next.Format("15:04:05")
		}
		out += fmt.Sprintf("  %s [%s] %s: %s%s\n", ev.Time.Format("01-02 15:04:05"), ev.State, ev.Type, ev.Detail, next)
	}
	return out
}

// TestMultipleCredits ensures targeting picks the soonest-expiring credit.
func TestMultipleCredits_PicksSoonestExpiring(t *testing.T) {
	E1 := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC) // sooner
	E2 := E1.Add(10 * 24 * time.Hour)                     // much later
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := &FakeOpenAIClient{
		Credits: map[string]*Credit{
			"later":   {ID: "later", Status: "available", ExpiresAt: E2},
			"sooner":  {ID: "sooner", Status: "available", ExpiresAt: E1},
		},
		WeeklyPct:   10,
		ConsumeCode: "reset",
		WindowsReset: 2,
	}
	client.OnConsumeReset = func(id string) {
		if cr, ok := client.Credits[id]; ok {
			cr.Status = "redeemed"
		}
		client.WeeklyPct = 100
	}

	// Start well before M so the first IDLE check decides target.
	start := E1.Add(-48 * time.Hour)
	out := Run(cfg, client, start, 14*24*time.Hour)

	if out.NumConsumePOST != 1 {
		t.Fatalf("expected 1 consume, got %d\n%v", out.NumConsumePOST, dumpEvents(out))
	}
	if len(out.ConsumedIDs) != 1 || out.ConsumedIDs[0] != "sooner" {
		t.Fatalf("expected 'sooner' to be consumed, got %v", out.ConsumedIDs)
	}
	// 'later' must still be available
	if cr, ok := client.Credits["later"]; !ok || cr.Status != "available" {
		t.Fatalf("later credit should remain available, got %v", client.Credits["later"])
	}
	t.Logf("OK consumed 'sooner', 'later' preserved, reset at %v", out.ResetAt)
}

// TestIdempotentRetry verifies a transient failure followed by success consumes
// the credit exactly once, reusing the same redeem_request_id.
func TestIdempotentRetry_FailureThenSuccess(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := newClient("c1", E, 20)
	// First POST fails with a transient error; the retry succeeds.
	client.FailFirstN = 1
	client.FailErr = fmt.Errorf("simulated network timeout")

	start := E.Add(-12 * time.Hour)
	out := Run(cfg, client, start, 14*24*time.Hour)

	if out.NumConsumePOST != 2 {
		t.Fatalf("expected 2 POSTs (1 fail + 1 success), got %d\n%v", out.NumConsumePOST, dumpEvents(out))
	}
	if out.GaveUp {
		t.Fatalf("should not have given up; reason=%q\n%v", out.GiveUpReason, dumpEvents(out))
	}
	if out.ResetAt.IsZero() {
		t.Fatalf("expected a reset time")
	}
	// Credit must be redeemed exactly once even though we POSTed twice.
	if cr, ok := client.Credits["c1"]; !ok || cr.Status != "redeemed" {
		t.Fatalf("c1 should be redeemed once, got %v", client.Credits["c1"])
	}
	// Both POSTs must carry the SAME redeem_request_id (idempotent retry).
	if len(client.RedeemRequests) != 2 {
		t.Fatalf("expected 2 redeem requests recorded, got %v", client.RedeemRequests)
	}
	if client.RedeemRequests[0] != client.RedeemRequests[1] {
		t.Fatalf("expected same redeem_request_id on retry, got %v", client.RedeemRequests)
	}
	t.Logf("OK retried with same UUID (%s); 2 POSTs, 1 redemption, reset=%v",
		client.RedeemRequests[0], out.ResetAt)
}

// TestConsumeNoCredit verifies the plugin stops cleanly when the server returns
// no_credit (credit list changed underneath us).
func TestConsumeNoCredit_Stops(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour, PostResetDelay: 1 * time.Minute}

	client := newClient("c1", E, 30)
	client.ConsumeCode = "no_credit"
	client.OnConsumeReset = nil // server doesn't deduct on no_credit

	start := E.Add(-12 * time.Hour)
	out := Run(cfg, client, start, 14*24*time.Hour)

	if out.NumConsumePOST != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", out.NumConsumePOST)
	}
	if !out.GaveUp {
		t.Fatalf("expected gaveUp=true on no_credit")
	}
	if out.GiveUpReason == "" {
		t.Fatalf("expected a give-up reason")
	}
	t.Logf("OK no_credit caused clean stop: %q", out.GiveUpReason)
}
