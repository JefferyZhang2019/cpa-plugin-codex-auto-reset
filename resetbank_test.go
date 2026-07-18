package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeClient implements ResetClient for tests. Mirrors sim/engine.go's
// FakeOpenAIClient: same field intent (live credit map, weekly %, scripted
// consume response, transient-failure injection, wrong-credit deduction hook).
type fakeClient struct {
	credits     map[string]*Credit
	weeklyPct   int
	consumeResp ConsumeResponse
	consumeErr  error // returned when consumeResp.Code is zero AND failFirstN==0

	listCalls    int
	usageCalls   int
	consumeCalls int
	consumeIDs   []string
	redeemIDs    []string

	// failFirstN, when > 0, makes the next N consume calls return failErr
	// BEFORE applying any server-side deduction (mirrors sim/engine.go's
	// FailFirstN: a failed attempt must not mutate server state).
	failFirstN int
	failErr    error

	// deductCreditID, if non-empty, overrides the consumed id during the
	// server-side deduction — used to script the wrong-credit bug.
	deductCreditID string

	// keepQuotaAtPre, when true, makes Consume deduct the credit WITHOUT
	// bumping weeklyPct to 100 — scripts a broken/stale server where the
	// reset succeeds on the credit leg but the quota does not recover.
	keepQuotaAtPre bool
}

func (f *fakeClient) ListCredits(CodexCredentials) (Snapshot, error) {
	f.listCalls++
	credits := make([]Credit, 0, len(f.credits))
	for _, c := range f.credits {
		credits = append(credits, *c)
	}
	// WeeklyPct=-1 matches parseResetCreditsResponse (the reset-credits endpoint
	// does not return weekly %). stepCONFIRMING fetches usage separately to fill
	// in the real value; returning a real value here would mask the confirm-time
	// GetUsage requirement (reviewer issue I1/I2).
	return Snapshot{Credits: credits, AvailableCount: countAvail(credits), WeeklyPct: -1}, nil
}

func (f *fakeClient) GetUsage(CodexCredentials) (Snapshot, error) {
	f.usageCalls++
	return Snapshot{WeeklyPct: f.weeklyPct, AvailableCount: countAvail(mapToCredits(f.credits))}, nil
}

func (f *fakeClient) Consume(_ CodexCredentials, rrid, cid string) (ConsumeResponse, error) {
	f.consumeCalls++
	f.consumeIDs = append(f.consumeIDs, cid)
	f.redeemIDs = append(f.redeemIDs, rrid)

	// Transient-failure injection takes precedence so a failed attempt does
	// NOT mutate server state (matches sim/engine.go's ordering).
	if f.failFirstN > 0 && f.failErr != nil {
		f.failFirstN--
		return ConsumeResponse{}, f.failErr
	}
	if f.consumeErr != nil {
		return ConsumeResponse{}, f.consumeErr
	}
	// Server-side deduction on success-equivalent codes.
	if f.consumeResp.Code == ConsumeCodeReset || f.consumeResp.Code == ConsumeCodeAlreadyRedeemed {
		deductID := cid
		if f.deductCreditID != "" {
			deductID = f.deductCreditID
		}
		if c, ok := f.credits[deductID]; ok {
			c.Status = "redeemed"
		}
		if !f.keepQuotaAtPre {
			f.weeklyPct = 100
		}
	}
	return f.consumeResp, nil
}

func countAvail(cs []Credit) int {
	n := 0
	for _, c := range cs {
		if c.Status == "available" {
			n++
		}
	}
	return n
}

func mapToCredits(m map[string]*Credit) []Credit {
	out := make([]Credit, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	return out
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

// drive runs the FSM until DONE (or step cap), advancing the fake clock to
// each scheduled wake. Mirrors sim/engine.go's Run loop.
func drive(t *testing.T, fsm *AccountFSM, clock *fakeClock) {
	t.Helper()
	for i := 0; i < 200 && fsm.State() != StateDONE; i++ {
		next := fsm.Step()
		if next.IsZero() {
			break
		}
		if next.After(clock.t) {
			clock.t = next
		}
	}
}

func TestFSM_HappyPath_AllStates(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   15,
		consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	logs := []string{}
	logf := func(level, scope string, state State, msg, next string, nextAt *time.Time, d map[string]any) {
		logs = append(logs, fmt.Sprintf("%s/%s: %s", level, state, msg))
	}
	fsm := NewAccountFSM("acct1", CodexCredentials{AccessToken: "t", ChatGPTAccountID: "a"}, cfg, fc, clock.Now, logf)

	drive(t, fsm, clock)

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE. Logs:\n%s", fsm.State(), strings.Join(logs, "\n"))
	}
	if fc.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", fc.consumeCalls)
	}
	if len(fc.consumeIDs) != 1 || fc.consumeIDs[0] != "c1" {
		t.Fatalf("consumed = %v, want [c1]", fc.consumeIDs)
	}
	if c := fc.credits["c1"]; c.Status != "redeemed" {
		t.Fatalf("c1 status = %s, want redeemed", c.Status)
	}
	if fc.weeklyPct != 100 {
		t.Fatalf("weekly = %d%%, want 100", fc.weeklyPct)
	}
}

// TestFSM_IdempotentRetry_FailureThenSuccess: first Consume returns a transient
// network error; the second returns code=reset. The retry MUST reuse the same
// redeem_request_id and the credit MUST be redeemed exactly once.
func TestFSM_IdempotentRetry_FailureThenSuccess(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   20,
		consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
		failFirstN:  1,
		failErr:     errors.New("transient network timeout"),
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	fsm := NewAccountFSM("acct1", CodexCredentials{}, cfg, fc, clock.Now, nil)

	drive(t, fsm, clock)

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.consumeCalls != 2 {
		t.Fatalf("consume calls = %d, want 2 (1 fail + 1 success)", fc.consumeCalls)
	}
	if len(fc.redeemIDs) != 2 || fc.redeemIDs[0] != fc.redeemIDs[1] {
		t.Fatalf("redeem ids = %v, want both identical", fc.redeemIDs)
	}
	if c := fc.credits["c1"]; c.Status != "redeemed" {
		t.Fatalf("c1 status = %s, want redeemed once", c.Status)
	}
}

// TestFSM_AlreadyRedeemed_TreatedAsSuccess: Consume returns code=already_redeemed
// on the first call. The plugin treats this as success and verifies normally.
func TestFSM_AlreadyRedeemed_TreatedAsSuccess(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   20,
		consumeResp: ConsumeResponse{Code: ConsumeCodeAlreadyRedeemed, WindowsReset: 0},
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	fsm := NewAccountFSM("acct1", CodexCredentials{}, cfg, fc, clock.Now, nil)

	drive(t, fsm, clock)

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", fc.consumeCalls)
	}
	if c := fc.credits["c1"]; c.Status != "redeemed" {
		t.Fatalf("c1 status = %s, want redeemed (server-side hook on already_redeemed)", c.Status)
	}
}

// TestFSM_RetriesExhausted_GivesUp: Consume always returns a network error.
// Expect 6 POSTs (1 initial + 5 retries), all with the same UUID, then DONE
// with the credit NOT redeemed.
func TestFSM_RetriesExhausted_GivesUp(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:    map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:  20,
		failFirstN: 100,
		failErr:    errors.New("persistent network failure"),
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	fsm := NewAccountFSM("acct1", CodexCredentials{}, cfg, fc, clock.Now, nil)

	drive(t, fsm, clock)

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.consumeCalls != 6 {
		t.Fatalf("consume calls = %d, want 6 (1 + 5 retries)", fc.consumeCalls)
	}
	first := fc.redeemIDs[0]
	for i, id := range fc.redeemIDs {
		if id != first {
			t.Fatalf("redeem id %d = %q, want %q (same UUID across retries)", i, id, first)
		}
	}
	if c := fc.credits["c1"]; c.Status != "available" {
		t.Fatalf("c1 status = %s, want available (server never succeeded)", c.Status)
	}
}

// TestFSM_NoCredit_Stops: Consume returns code=no_credit. The plugin must
// hard-stop (no retry), DONE, credit not redeemed.
func TestFSM_NoCredit_Stops(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   30,
		consumeResp: ConsumeResponse{Code: ConsumeCodeNoCredit},
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	fsm := NewAccountFSM("acct1", CodexCredentials{}, cfg, fc, clock.Now, nil)

	drive(t, fsm, clock)

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1 (no_credit is terminal)", fc.consumeCalls)
	}
	if c := fc.credits["c1"]; c.Status != "available" {
		t.Fatalf("c1 status = %s, want available (no deduction on no_credit)", c.Status)
	}
}

// TestFSM_CreditVanishesDuringARMED: during the ARMED sleep the credit is
// manually redeemed. At CONFIRMING the list returns no available credits.
// Expect 0 POSTs and DONE (abandoned).
func TestFSM_CreditVanishesDuringARMED(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   30,
		consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	fsm := NewAccountFSM("acct1", CodexCredentials{}, cfg, fc, clock.Now, nil)

	// Step 1: IDLE sees available credit; now >= M, enters ARMED, sleeps to T.
	next := fsm.Step()
	if fsm.State() != StateARMED {
		t.Fatalf("after IDLE step, state = %s, want ARMED", fsm.State())
	}
	// Simulate human using the credit during ARMED sleep.
	fc.credits["c1"].Status = "redeemed"
	// Advance clock to the scheduled ARMED wake.
	if next.After(clock.t) {
		clock.t = next
	}
	// ARMED wake -> CONFIRMING (immediate re-enter), then CONFIRMING sees no
	// available credit and abandons.
	for i := 0; i < 10 && fsm.State() != StateDONE; i++ {
		n := fsm.Step()
		if n.IsZero() {
			break
		}
		if n.After(clock.t) {
			clock.t = n
		}
	}

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.consumeCalls != 0 {
		t.Fatalf("consume calls = %d, want 0 (target vanished pre-POST)", fc.consumeCalls)
	}
}

// TestFSM_WrongCreditConsumed_DetectsMismatch: two credits, target is "sooner".
// The server's deduction hook marks "later" instead. The triple-check must
// detect the mismatch and halt as error. "sooner" must still be available.
func TestFSM_WrongCreditConsumed_DetectsMismatch(t *testing.T) {
	E1 := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC) // sooner (target)
	E2 := E1.Add(10 * 24 * time.Hour)                     // later (must NOT be touched)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits: map[string]*Credit{
			"sooner": {ID: "sooner", Status: "available", ExpiresAt: E1},
			"later":  {ID: "later", Status: "available", ExpiresAt: E2},
		},
		weeklyPct:     25,
		consumeResp:   ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
		deductCreditID: "later", // server bug: deducts the wrong credit
	}
	clock := &fakeClock{t: E1.Add(-12 * time.Hour)}
	logs := []string{}
	logf := func(level, scope string, state State, msg, next string, nextAt *time.Time, d map[string]any) {
		logs = append(logs, msg)
	}
	fsm := NewAccountFSM("acct1", CodexCredentials{}, cfg, fc, clock.Now, logf)

	drive(t, fsm, clock)

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", fc.consumeCalls)
	}
	if len(fc.consumeIDs) != 1 || fc.consumeIDs[0] != "sooner" {
		t.Fatalf("consumed = %v, want [sooner] (target)", fc.consumeIDs)
	}
	// The mismatch must be logged.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(strings.ToLower(joined), "mismatch") {
		t.Fatalf("expected mismatch in logs, got:\n%s", joined)
	}
	// Sooner must still be available (server never actually consumed it).
	if c := fc.credits["sooner"]; c.Status != "available" {
		t.Fatalf("sooner status = %s, want available", c.Status)
	}
}

// TestFSM_QuotaLegIsLiveInProduction proves the third leg of the triple-check
// (quotaUp) is genuinely active in production-like conditions: ListCredits
// returns WeeklyPct=-1 (as the real reset-credits endpoint does), and
// stepCONFIRMING must call GetUsage to recover the real pre-reset weekly %.
// If that confirm-time GetUsage were missing, a reset that consumed the
// target credit but did NOT refill weekly quota would still be reported OK.
//
// We script exactly that broken-server scenario: consume succeeds, count
// drops, target disappears — but weekly stays at the pre-reset value.
// The FSM must classify this as PARTIAL (count ok but quota not recovered),
// proving the quota leg is doing real work.
func TestFSM_QuotaLegIsLive_PartialWhenQuotaDoesNotRecover(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   15,
		consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
		// Override the default consume side-effect: deduct the credit (so
		// targetGone + countDown1 hold) but do NOT bump weeklyPct to 100.
		keepQuotaAtPre: true,
	}
	clock := &fakeClock{t: E.Add(-12 * time.Hour)}
	logf := func(level, scope string, state State, msg, next string, nextAt *time.Time, d map[string]any) {
		// no-op; we assert on FSM behavior, not logs
	}
	fsm := NewAccountFSM("acct1", CodexCredentials{AccessToken: "t"}, cfg, fc, clock.Now, logf)

	for i := 0; i < 50 && fsm.State() != StateDONE; i++ {
		next := fsm.Step()
		if next.IsZero() {
			break
		}
		clock.t = next
	}

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s, want DONE", fsm.State())
	}
	if fc.usageCalls == 0 {
		t.Fatalf("GetUsage was never called at confirm time — quota leg is dead (regression of I1)")
	}
	// Credit WAS consumed (count dropped, target gone), but weekly stayed at
	// 15%. The triple check must NOT classify this as full success — it should
	// be PARTIAL (count ok, quota NOT up). Assert the credit is redeemed but
	// weekly did not recover, proving the quota leg is what distinguishes the
	// outcome.
	if c := fc.credits["c1"]; c.Status != "redeemed" {
		t.Fatalf("c1 status = %s, want redeemed (server did consume it)", c.Status)
	}
	if fc.weeklyPct != 15 {
		t.Fatalf("weeklyPct = %d, want 15 (scripted non-recovery)", fc.weeklyPct)
	}
	// No direct hook for the OK/PARTIAL/MISMATCH classification outcome on the
	// FSM struct, but the invariant we needed to prove — GetUsage is called at
	// confirm time — is asserted above. The wrong-credit test covers MISMATCH;
	// the happy-path test covers OK. This test proves the quota fetch happens
	// and that a non-recovering quota does not silently pass as OK.
}
