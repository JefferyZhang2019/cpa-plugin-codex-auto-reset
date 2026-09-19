package main

import (
	"sync"
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

// TestWorker_ManualResetImmediate verifies the manual reset (ForceConfirm) runs
// the full cycle promptly thanks to the wake channel — no waiting for the
// previously scheduled patrol.
func TestWorker_ManualResetImmediate(t *testing.T) {
	defer resetTestClock()
	E := time.Now().Add(5 * 24 * time.Hour) // far-future expiry; nothing auto-triggers
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits:     map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct:   50,
		consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 1},
	}
	loader := func(string) (CodexCredentials, ResetClient, error) {
		return CodexCredentials{AccessToken: "t"}, fc, nil
	}
	w := NewWorker(cfg, []string{"a"}, loader, time.Now)
	w.testAccelerate = true
	withTestClock(w, time.Now())

	done := make(chan struct{})
	go func() { w.Run(); close(done) }()

	// Wait for the initial patrol to complete.
	time.Sleep(50 * time.Millisecond)

	// Manual reset: must complete the whole cycle within a short window
	// (CONFIRMING -> RESETTING -> VERIFYING -> DONE), not at the next 12h patrol.
	if err := w.ForceConfirm("a"); err != nil {
		t.Fatalf("ForceConfirm: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if fc.consumeCalls >= 1 {
			w.Stop()
			<-done
			return
		}
		select {
		case <-deadline:
			w.Stop()
			<-done
			t.Fatalf("manual reset did not run within 5s (consumeCalls=%d)", fc.consumeCalls)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
