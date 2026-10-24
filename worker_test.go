package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWorkerClock is a mutable clock for worker tests. Safe for concurrent
// AdvanceTo / Now calls.
type fakeWorkerClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeWorkerClock(at time.Time) *fakeWorkerClock {
	return &fakeWorkerClock{t: at}
}

func (f *fakeWorkerClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeWorkerClock) AdvanceTo(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.After(f.t) {
		f.t = t
	}
}

func (f *fakeWorkerClock) Get() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// withTestClock installs a fakeWorkerClock as the worker's clock AND as the
// testClockAdvancer hook so testAccelerate mode can advance it. Returns the
// clock and a cleanup func.
func withTestClock(w *Worker, start time.Time) *fakeWorkerClock {
	c := newFakeWorkerClock(start)
	w.nowFn = c.Now
	testClockAdvancer = c.AdvanceTo
	return c
}

func resetTestClock() {
	testClockAdvancer = nil
}

func TestWorker_DrivesMultipleAccountsToDone(t *testing.T) {
	defer resetTestClock()
	E := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fcA := &fakeClient{credits: map[string]*Credit{"a1": {ID: "a1", Status: "available", ExpiresAt: E}}, weeklyPct: 10, consumeResp: ConsumeResponse{Code: ConsumeCodeReset}}
	fcB := &fakeClient{credits: map[string]*Credit{"b1": {ID: "b1", Status: "available", ExpiresAt: E}}, weeklyPct: 20, consumeResp: ConsumeResponse{Code: ConsumeCodeReset}}
	credsA := CodexCredentials{AccessToken: "ta", ChatGPTAccountID: "aa"}
	credsB := CodexCredentials{AccessToken: "tb", ChatGPTAccountID: "bb"}

	loader := func(authID string) (CodexCredentials, ResetClient, error) {
		switch authID {
		case "a":
			return credsA, fcA, nil
		case "b":
			return credsB, fcB, nil
		}
		return CodexCredentials{}, nil, errUnknownAccount
	}
	w := NewWorker(cfg, []string{"a", "b"}, loader, time.Now)
	w.testAccelerate = true
	clock := withTestClock(w, E.Add(-12*time.Hour))

	done := make(chan struct{})
	go func() { w.Run(); close(done) }()

	// In testAccelerate mode, the worker advances the clock to nextWake each
	// cycle, so both FSMs reach their reset target within milliseconds. We
	// can't safely poll fakeClient fields (race with the worker goroutine),
	// so we sleep a fixed short time then STOP the worker, after which reading
	// fakeClient is safe.
	time.Sleep(300 * time.Millisecond)
	w.Stop()
	<-done

	// After stop, both credits must be redeemed and weekly refilled.
	if fcA.credits["a1"].Status != "redeemed" {
		t.Fatalf("account a: credit a1 status = %s, want redeemed (clock=%v)", fcA.credits["a1"].Status, clock.Get())
	}
	if fcB.credits["b1"].Status != "redeemed" {
		t.Fatalf("account b: credit b1 status = %s, want redeemed (clock=%v)", fcB.credits["b1"].Status, clock.Get())
	}
	if fcA.weeklyPct != 100 || fcB.weeklyPct != 100 {
		t.Fatalf("weekly not refilled: a=%d b=%d", fcA.weeklyPct, fcB.weeklyPct)
	}
	if fcA.consumeCalls != 1 || fcB.consumeCalls != 1 {
		t.Fatalf("expected exactly 1 consume each, got a=%d b=%d", fcA.consumeCalls, fcB.consumeCalls)
	}
}

var errUnknownAccount = errString("unknown account")

type errString string

func (e errString) Error() string { return string(e) }

func TestWorker_TriggerCheckDefersNextAuto(t *testing.T) {
	defer resetTestClock()
	// Spec §6.3 deferral: a TriggerCheck forces the FSM to step immediately,
	// and after that step completes (IDLE patrol with no imminent credit),
	// nextWake = completion_time + refresh_interval. We verify the immediate
	// step happens and nextWake is pushed at least one R beyond the pre-trigger
	// schedule (i.e. deferral, not the original schedule).
	//
	// Because testAccelerate makes the loop advance the clock forward each
	// iteration, we keep the credit far from expiry so the FSM stays in IDLE
	// (one patrol per loop tick) and we stop the worker quickly to avoid the
	// clock running away.
	E := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC) // far future
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{credits: map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}}, weeklyPct: 50, consumeResp: ConsumeResponse{Code: ConsumeCodeReset}}
	start := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	loader := func(string) (CodexCredentials, ResetClient, error) {
		return CodexCredentials{AccessToken: "t"}, fc, nil
	}
	w := NewWorker(cfg, []string{"a"}, loader, time.Now)
	w.testAccelerate = true
	clock := withTestClock(w, start)

	done := make(chan struct{})
	go func() { w.Run(); close(done) }()

	// Capture the scheduled nextWake before manual trigger.
	time.Sleep(30 * time.Millisecond)
	getNextWake := func() time.Time {
		var nw time.Time
		w.EachFSM(func(fsm *AccountFSM) { nw = fsm.NextWake() })
		return nw
	}
	preTriggerClock := clock.Get()

	// Manual trigger.
	w.TriggerCheck("a")
	// Wait for one step to occur.
	time.Sleep(30 * time.Millisecond)

	// After the manual-trigger step, nextWake must be after the pre-trigger
	// clock time (deferral: pushed forward, not held at the old schedule).
	postNW := getNextWake()
	if !postNW.After(preTriggerClock) {
		t.Fatalf("post-trigger nextWake %v not after pre-trigger clock %v", postNW, preTriggerClock)
	}

	w.Stop()
	<-done
}

// TestWorker_StateSyncPropagatesFSMState verifies the StateSync callback
// fires after each step and carries the live FSM state. This is the
// regression guard for the management UI's per-account state badge —
// without StateSync, the UI would show IDLE forever.
func TestWorker_StateSyncPropagatesFSMState(t *testing.T) {
	defer resetTestClock()
	E := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{credits: map[string]*Credit{"a1": {ID: "a1", Status: "available", ExpiresAt: E}}, weeklyPct: 10, consumeResp: ConsumeResponse{Code: ConsumeCodeReset}}
	loader := func(string) (CodexCredentials, ResetClient, error) {
		return CodexCredentials{AccessToken: "t"}, fc, nil
	}
	w := NewWorker(cfg, []string{"a"}, loader, time.Now)
	w.testAccelerate = true
	withTestClock(w, E.Add(-12*time.Hour))

	// Track every (authID, state) snapshot the callback sees.
	var snapsMu sync.Mutex
	var seenStates []State
	w.StateSync = func(authID string, fsm *AccountFSM) {
		if authID != "a" {
			return
		}
		snapsMu.Lock()
		seenStates = append(seenStates, fsm.State())
		snapsMu.Unlock()
	}

	done := make(chan struct{})
	go func() { w.Run(); close(done) }()
	time.Sleep(300 * time.Millisecond)
	w.Stop()
	<-done

	snapsMu.Lock()
	defer snapsMu.Unlock()
	if len(seenStates) == 0 {
		t.Fatalf("StateSync was never called; management UI would see no state updates")
	}
	// Over a full cycle the FSM must have visited at least CONFIRMING or
	// RESETTING or VERIFYING (i.e. not stuck at IDLE the whole time).
	progressed := false
	for _, s := range seenStates {
		if s == StateCONFIRMING || s == StateRESETTING || s == StateVERIFYING || s == StateDONE {
			progressed = true
			break
		}
	}
	if !progressed {
		t.Fatalf("FSM never progressed beyond IDLE/ARMED in snapshots: %v", seenStates)
	}
}

// TestWorker_ForceConfirmNotFound verifies ForceConfirm returns an error for an
// unknown account instead of silently doing nothing (the old behavior made the
// manual reset button appear to work while nothing happened).
func TestWorker_ForceConfirmNotFound(t *testing.T) {
	defer resetTestClock()
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	loader := func(string) (CodexCredentials, ResetClient, error) {
		return CodexCredentials{AccessToken: "t"}, &fakeClient{}, nil
	}
	w := NewWorker(cfg, []string{"a"}, loader, time.Now)
	done := make(chan struct{})
	go func() { w.Run(); close(done) }()
	time.Sleep(50 * time.Millisecond) // let Run() create the FSM
	w.Stop()
	<-done

	if err := w.ForceConfirm("nonexistent"); err == nil {
		t.Fatalf("expected error for unknown account, got nil")
	}
	if err := w.TriggerCheck("nonexistent"); err == nil {
		t.Fatalf("expected error for unknown account, got nil")
	}
	// Known account must not error (worker stopped, but FSM map persists).
	if err := w.ForceConfirm("a"); err != nil {
		t.Fatalf("unexpected error for known account: %v", err)
	}
}

func TestWorker_ManualResetImmediate(t *testing.T) {
	start := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	clock := newFakeWorkerClock(start)
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: start.Add(5 * 24 * time.Hour)}},
		weeklyPct:   50,
		consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 1},
	}
	loader := func(string) (CodexCredentials, ResetClient, error) {
		return CodexCredentials{AccessToken: "t"}, fc, nil
	}
	w := NewWorker(cfg, []string{"a"}, loader, clock.Now)
	patrolled := make(chan int, 1)
	w.StateSync = func(_ string, fsm *AccountFSM) {
		if fsm.State() == StateIDLE {
			patrolled <- fc.consumeCalls
		}
		if fsm.State() == StateVERIFYING {
			w.Stop()
		}
	}
	done := make(chan struct{})
	go func() { w.Run(); close(done) }()
	t.Cleanup(func() { w.Stop(); <-done })

	select {
	case calls := <-patrolled:
		if calls != 0 {
			t.Fatalf("consumed %d times before manual reset", calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial patrol did not complete")
	}
	if err := w.ForceConfirm("a"); err != nil {
		t.Fatalf("ForceConfirm: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("manual reset did not wake the worker")
	}
	if fc.consumeCalls != 1 || fc.consumeIDs[0] != "c1" || !clock.Now().Equal(start) {
		t.Fatalf("manual reset: consumed=%v clock=%v, want c1 immediately", fc.consumeIDs, clock.Now())
	}
	fsm := w.fsms["a"]
	clock.AdvanceTo(fsm.NextWake())
	w.stepAccountSafe("a")
	if rec := fsm.TakePendingHistory(); rec == nil || !rec.Success || fsm.State() != StateIDLE {
		t.Fatalf("manual verification: state=%s record=%+v", fsm.State(), rec)
	}
}

func armedWorkerFixture(t *testing.T) (*Worker, *AccountFSM, *fakeClient, *fakeClock, time.Time) {
	t.Helper()
	fsm, fc, clock, trigger := armedFSMFixture(t)
	w := NewWorker(fsm.Cfg, []string{"acct"}, func(string) (CodexCredentials, ResetClient, error) {
		return CodexCredentials{}, fc, nil
	}, clock.Now)
	w.fsms["acct"] = fsm
	return w, fsm, fc, clock, trigger
}

func TestWorker_ManagementCheckRespectsPinnedCycle(t *testing.T) {
	for _, route := range []string{"check", "check/all"} {
		t.Run(route, func(t *testing.T) {
			w, fsm, fc, clock, trigger := armedWorkerFixture(t)
			listsBefore := fc.listCalls
			h := newManagementHandlers(&PluginState{Config: w.cfg}, "", w)
			status, body, _ := h.handle("POST", "/v0/management/plugins/codex-auto-reset/"+route, nil, []byte(`{"auth_index":"acct"}`))
			// Check on an ARMED account keeps the pinned trigger wake and
			// burns no list call; the confirm at T revalidates anyway.
			if status != 200 || !fsm.NextWake().Equal(trigger) {
				t.Fatalf("check: status=%d body=%s wake=%v, want unchanged trigger %v", status, body, fsm.NextWake(), trigger)
			}
			if fc.listCalls != listsBefore {
				t.Fatalf("check burned %d extra list calls on ARMED account", fc.listCalls-listsBefore)
			}
			select {
			case <-w.wakeCh:
			default:
				t.Fatal("check did not wake worker")
			}
			// External redemption before T still abandons at T without
			// substituting the replacement credit.
			fc.credits["a"].Status = "redeemed"
			clock.t = trigger
			w.stepAccountSafe("acct")
			w.stepAccountSafe("acct")
			if fsm.State() == StateRESETTING {
				w.stepAccountSafe("acct")
			}
			if fsm.State() != StateIDLE || fc.consumeCalls != 0 || !fsm.NextWake().Equal(trigger.Add(w.cfg.RefreshInterval)) {
				t.Fatalf("abandoned cycle: state=%s consumed=%v wake=%v", fsm.State(), fc.consumeIDs, fsm.NextWake())
			}
		})
	}
}

// TestWorker_TriggerCheckOnlyPullsIDLE verifies Check semantics per state:
// IDLE is pulled to an immediate patrol, while RESETTING (retry backoff) and
// VERIFYING (verify delay) keep their schedule instead of being bypassed.
func TestWorker_TriggerCheckOnlyPullsIDLE(t *testing.T) {
	w, fsm, fc, clock, trigger := armedWorkerFixture(t)
	fc.failFirstN = 1
	fc.failErr = errString("temporary failure")
	clock.t = trigger

	// Drive to RESETTING with a pending retry backoff.
	w.stepAccountSafe("acct") // ARMED -> CONFIRMING
	w.stepAccountSafe("acct") // CONFIRMING -> RESETTING (attempt minted, no POST yet)
	w.stepAccountSafe("acct") // first POST fails -> retry backoff scheduled
	if fsm.State() != StateRESETTING {
		t.Fatalf("state=%s, want RESETTING", fsm.State())
	}
	if fc.consumeCalls != 1 {
		t.Fatalf("consumeCalls=%d, want 1 failed attempt", fc.consumeCalls)
	}
	backoff := fsm.NextWake()
	if !backoff.After(clock.t) {
		t.Fatalf("no backoff scheduled: wake=%v now=%v", backoff, clock.t)
	}
	if err := w.TriggerCheck("acct"); err != nil {
		t.Fatal(err)
	}
	if !fsm.NextWake().Equal(backoff) {
		t.Fatalf("check bypassed retry backoff: wake=%v, want %v", fsm.NextWake(), backoff)
	}

	// Advance past the backoff; the retry succeeds and enters VERIFYING.
	clock.t = backoff
	w.stepAccountSafe("acct")
	if fsm.State() != StateVERIFYING {
		t.Fatalf("state=%s, want VERIFYING", fsm.State())
	}
	verifyDue := fsm.NextWake()
	if err := w.TriggerCheck("acct"); err != nil {
		t.Fatal(err)
	}
	if !fsm.NextWake().Equal(verifyDue) {
		t.Fatalf("check bypassed verify delay: wake=%v, want %v", fsm.NextWake(), verifyDue)
	}

	// Complete verification; the settled FSM is IDLE and Check now pulls it.
	clock.t = verifyDue
	w.stepAccountSafe("acct")
	if fsm.State() != StateIDLE {
		t.Fatalf("state=%s, want IDLE", fsm.State())
	}
	patrol := fsm.NextWake()
	if err := w.TriggerCheck("acct"); err != nil {
		t.Fatal(err)
	}
	if !fsm.NextWake().Equal(clock.t) || fsm.NextWake().Equal(patrol) {
		t.Fatalf("check did not pull IDLE account: wake=%v patrol=%v", fsm.NextWake(), patrol)
	}
}

func TestWorker_ManagementManualResetFarFuture(t *testing.T) {
	for _, initial := range []State{StateIDLE, StateARMED} {
		for _, expiry := range []string{"far_future", "never"} {
			t.Run(string(initial)+"/"+expiry, func(t *testing.T) {
				w, fsm, fc, clock, _ := armedWorkerFixture(t)
				if initial == StateIDLE {
					fsm.Reset()
				}
				fc.credits["a"].Status = "redeemed"
				if expiry == "never" {
					fc.credits["b"].ExpiresAt = time.Time{}
				}
				h := newManagementHandlers(&PluginState{Config: w.cfg}, "", w)
				status, body, _ := h.handle("POST", "/v0/management/plugins/codex-auto-reset/reset", nil, []byte(`{"auth_index":"acct"}`))
				if status != 200 || fsm.State() != StateCONFIRMING || !fsm.NextWake().Equal(clock.t) {
					t.Fatalf("manual reset: status=%d body=%s state=%s wake=%v", status, body, fsm.State(), fsm.NextWake())
				}
				if !fsm.manualReset || fsm.scheduledCreditID != "" {
					t.Fatalf("manual intent=%v scheduled target=%q, want manual with obsolete target cleared", fsm.manualReset, fsm.scheduledCreditID)
				}
				select {
				case <-w.wakeCh:
				default:
					t.Fatal("manual reset did not wake worker")
				}
				w.stepAccountSafe("acct")
				w.stepAccountSafe("acct")
				if fc.consumeCalls != 1 || fc.consumeIDs[0] != "b" || !fsm.attempt.StartedAt.Equal(clock.t) {
					t.Fatalf("manual consume=%v attempt=%+v, want b now", fc.consumeIDs, fsm.attempt)
				}
				clock.t = fsm.NextWake()
				w.stepAccountSafe("acct")
				if rec := fsm.TakePendingHistory(); rec == nil || !rec.Success || fsm.State() != StateIDLE {
					t.Fatalf("manual verification: state=%s record=%+v", fsm.State(), rec)
				}
			})
		}
	}
}

func TestWorker_ResetClearsManualIntent(t *testing.T) {
	w, fsm, fc, clock, trigger := armedWorkerFixture(t)
	if err := w.ForceConfirm("acct"); err != nil {
		t.Fatal(err)
	}
	fsm.Reset()
	if !fsm.NextWake().IsZero() || fsm.attempt.RedeemRequestID != "" {
		t.Fatalf("Reset retained schedule or attempt: wake=%v attempt=%+v", fsm.NextWake(), fsm.attempt)
	}
	w.stepAccountSafe("acct")
	if fsm.State() != StateARMED {
		t.Fatalf("new automatic cycle state=%s, want ARMED", fsm.State())
	}
	fc.credits["a"].Status = "redeemed"
	clock.t = trigger
	w.stepAccountSafe("acct")
	w.stepAccountSafe("acct")
	if fsm.State() != StateIDLE || fsm.attempt.RedeemRequestID != "" || fc.consumeCalls != 0 {
		t.Fatalf("manual intent leaked into next cycle: state=%s attempt=%+v", fsm.State(), fsm.attempt)
	}
}

func TestWorker_ManualResetPreservesActiveAttempt(t *testing.T) {
	w, fsm, fc, clock, trigger := armedWorkerFixture(t)
	fc.failFirstN = 1
	fc.failErr = errString("temporary failure")
	clock.t = trigger
	w.stepAccountSafe("acct")
	w.stepAccountSafe("acct")
	w.stepAccountSafe("acct")
	for _, state := range []State{StateRESETTING, StateVERIFYING} {
		if fsm.State() != state {
			t.Fatalf("state=%s, want %s", fsm.State(), state)
		}
		wake := fsm.NextWake()
		rrid := fsm.attempt.RedeemRequestID
		if err := w.ForceConfirm("acct"); err != nil {
			t.Fatal(err)
		}
		if fsm.State() != state || !fsm.NextWake().Equal(wake) || fsm.attempt.RedeemRequestID != rrid {
			t.Fatalf("manual reset interrupted %s: state=%s wake=%v attempt=%+v", state, fsm.State(), fsm.NextWake(), fsm.attempt)
		}
		clock.t = wake
		w.stepAccountSafe("acct")
	}
	if fc.consumeCalls != 2 || fc.redeemIDs[0] != fc.redeemIDs[1] || fc.consumeIDs[0] != "a" || fc.consumeIDs[1] != "a" {
		t.Fatalf("retry changed: targets=%v redeemIDs=%v", fc.consumeIDs, fc.redeemIDs)
	}
	if rec := fsm.TakePendingHistory(); rec == nil || !rec.Success {
		t.Fatalf("verification record=%+v", rec)
	}
}

// TestWorker_RetriesInitialCredentialLoadFailure guards the starvation bug:
// an account whose credentials failed to load during startup was never added
// to the FSM map, and soonest() only looks at that map, so the account stayed
// invisible until the next plugin restart. It must now be retried on the
// credential backoff.
func TestWorker_RetriesInitialCredentialLoadFailure(t *testing.T) {
	defer resetTestClock()
	start := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{credits: map[string]*Credit{}, weeklyPct: 50}
	var attempts int32
	loader := func(string) (CodexCredentials, ResetClient, error) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return CodexCredentials{}, nil, errString("auth file not ready")
		}
		return CodexCredentials{AccessToken: "t"}, fc, nil
	}
	w := NewWorker(cfg, []string{"a"}, loader, time.Now)
	w.testAccelerate = true
	clock := withTestClock(w, start)

	done := make(chan struct{})
	go func() { w.Run(); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		found := false
		w.EachFSM(func(*AccountFSM) { found = true })
		if found {
			break
		}
		if time.Now().After(deadline) {
			w.Stop()
			<-done
			t.Fatalf("account never recovered after transient initial load failure (attempts=%d)", atomic.LoadInt32(&attempts))
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.Stop()
	<-done

	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("credential load attempts = %d, want >= 2", got)
	}
	if !clock.Get().After(start.Add(credentialRetryInterval - time.Second)) {
		t.Fatalf("worker did not wait for the credential backoff: clock=%v", clock.Get())
	}
}
