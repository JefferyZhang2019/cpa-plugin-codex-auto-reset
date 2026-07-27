package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// CredsLoader returns the current credentials and an OpenAI client for an
// account. The worker calls this before every step to pick up CPA-side token
// refreshes (spec §6.2).
type CredsLoader func(authID string) (CodexCredentials, ResetClient, error)

// Worker drives one AccountFSM per enabled account from a single goroutine.
// It picks the soonest nextWake across accounts, sleeps until then, steps
// that account, and recomputes. Manual triggers (TriggerCheck, ForceConfirm)
// wake the loop early.
//
// Concurrency contract: the worker goroutine owns the FSM structs after init.
// EachFSM, TriggerCheck, ForceConfirm, and Stop are safe to call concurrently
// from other goroutines (management handlers); they take the worker mutex and
// only read/snapshot FSM fields. Reading an FSM's State()/NextWake() from
// another goroutine may observe a stale value, which is harmless (the next
// poll reconciles).
type Worker struct {
	cfg     Config
	enabled []string
	loader  CredsLoader
	nowFn   func() time.Time

	// StateSync, if non-nil, is called after every FSM step so the caller
	// (main.go's configurePlugin) can mirror FSM state into the persisted
	// PluginState for the management UI and crash recovery. Called inline
	// from the worker goroutine.
	StateSync func(authID string, fsm *AccountFSM)

	mu      sync.Mutex
	fsms    map[string]*AccountFSM
	logs    *logRing
	stopCh  chan struct{}
	stopped bool

	// testAccelerate, when true, makes Run advance the injected clock to the
	// next wake time instead of sleeping in real time. Lets tests run a full
	// multi-day FSM cycle in milliseconds. Production leaves this false.
	testAccelerate bool
}

func NewWorker(cfg Config, enabled []string, loader CredsLoader, nowFn func() time.Time) *Worker {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Worker{
		cfg: cfg, enabled: enabled, loader: loader, nowFn: nowFn,
		fsms: map[string]*AccountFSM{}, logs: newLogRing(maxLogEntries), stopCh: make(chan struct{}),
	}
}

func (w *Worker) Logs() *logRing { return w.logs }

// EachFSM iterates accounts in deterministic (sorted) order so test assertions
// are stable. Snapshots the FSM pointers under lock; the callback must not
// block (it is typically a read-only State()/NextWake() check).
func (w *Worker) EachFSM(fn func(*AccountFSM)) {
	w.mu.Lock()
	keys := make([]string, 0, len(w.fsms))
	for k := range w.fsms {
		keys = append(keys, k)
	}
	snap := make([]*AccountFSM, 0, len(keys))
	for _, k := range keys {
		if fsm, ok := w.fsms[k]; ok {
			snap = append(snap, fsm)
		}
	}
	w.mu.Unlock()
	sort.Strings(keys)
	// Re-iterate in sorted order using the snapshot map.
	byKey := map[string]*AccountFSM{}
	for i, k := range keys {
		if i < len(snap) {
			byKey[k] = snap[i]
		}
	}
	for _, k := range keys {
		if fsm, ok := byKey[k]; ok {
			fn(fsm)
		}
	}
}

func (w *Worker) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	close(w.stopCh)
	w.mu.Unlock()
}

// TriggerCheck forces nextWake=now for one account (or all, if authID is
// empty), so the worker loop picks it up on the next tick. In production the
// loop may be sleeping until a later nextWake; the trigger lowers it to now
// but the loop only notices when its current timer fires or the stop channel
// fires. For immediate wakeup in production, callers should also send on an
// external wake channel — but for the manual-check button the small latency
// (up to the current sleep) is acceptable and tests use testAccelerate.
func (w *Worker) TriggerCheck(authID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.nowFn()
	if authID == "" {
		for _, fsm := range w.fsms {
			fsm.SetNextWake(now)
		}
		return
	}
	if fsm, ok := w.fsms[authID]; ok {
		fsm.SetNextWake(now)
	}
}

// ForceConfirm advances an account's FSM directly to CONFIRMING (manual reset
// button). Still runs the full confirm + idempotent consume + verify flow.
func (w *Worker) ForceConfirm(authID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if fsm, ok := w.fsms[authID]; ok {
		fsm.ForceState(StateCONFIRMING)
		fsm.SetNextWake(w.nowFn())
	}
}

// Run blocks until Stop. Initializes FSMs, then loops: pick the soonest
// nextWake, sleep (or advance the clock if testAccelerate), step that account,
// refresh its credentials first.
//
// CRITICAL: the entire loop body is wrapped in a defer/recover so that a
// panic in any single account's Step() (nil pointer, bad JSON, network
// timeout, etc.) does NOT kill the worker goroutine and strand all other
// accounts forever. The panicking account gets its nextWake pushed forward
// and the loop continues.
func (w *Worker) Run() {
	defer func() {
		if r := recover(); r != nil {
			// This should never happen — the inner recover should catch per-account
			// panics. But if something in the loop infrastructure panics, log it
			// rather than dying silently.
			w.logs.append(LogEntry{
				Timestamp: w.nowFn(), Level: "error", Scope: "system",
				Message: fmt.Sprintf("worker Run() panic (outer): %v", r),
			})
		}
	}()

	w.mu.Lock()
	for _, id := range w.enabled {
		w.ensureFSM(id)
	}
	w.mu.Unlock()

	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		nextAcct, nextWake := w.soonest()
		if nextAcct == "" {
			// Nothing scheduled yet (e.g. all accounts failed initial cred
			// load). Poll briefly so we don't spin a core.
			w.sleepOrStop(100 * time.Millisecond)
			continue
		}
		now := w.nowFn()
		if nextWake.After(now) {
			if w.testAccelerate {
				w.advanceClock(nextWake)
			} else {
				w.sleepOrStop(nextWake.Sub(now))
			}
			continue
		}

		// Step ONE account with full panic recovery.
		w.stepAccountSafe(nextAcct)
	}
}

// stepAccountSafe steps a single account, recovering from any panic so the
// worker goroutine survives. On panic, the account gets nextWake pushed
// forward by refresh_interval and an error log is recorded.
func (w *Worker) stepAccountSafe(authID string) {
	defer func() {
		if r := recover(); r != nil {
			w.logs.append(LogEntry{
				Timestamp: w.nowFn(), Level: "error", Scope: authID,
				Message: fmt.Sprintf("worker panic recovered: %v", r),
			})
			// Push the account's next wake forward so it retries later.
			w.mu.Lock()
			if fsm, ok := w.fsms[authID]; ok {
				fsm.SetNextWake(w.nowFn().Add(w.cfg.RefreshInterval))
			}
			w.mu.Unlock()
		}
	}()

	w.mu.Lock()
	fsm := w.fsms[authID]
	w.mu.Unlock()
	if fsm == nil {
		w.ensureFSM(authID)
		return
	}

	// Refresh credentials before stepping.
	creds, client, err := w.loader(authID)
	if err != nil {
		w.logs.append(LogEntry{
			Timestamp: w.nowFn(), Level: "error", Scope: authID,
			State: fsm.State(), Message: "credential load failed: " + err.Error(),
		})
		fsm.SetNextWake(w.nowFn().Add(w.cfg.RefreshInterval))
		return
	}

	// Set credentials and client under the FSM mutex to avoid data races
	// with concurrent State()/NextWake() readers.
	fsm.mu.Lock()
	fsm.Creds = creds
	fsm.Client = client
	fsm.mu.Unlock()

	wake := fsm.Step()
	if wake.IsZero() && fsm.State() == StateDONE {
		fsm.Reset()
		fsm.SetNextWake(w.nowFn().Add(w.cfg.RefreshInterval))
	} else {
		fsm.SetNextWake(wake)
	}

	// Mirror FSM state into the persisted PluginState.
	if w.StateSync != nil {
		w.StateSync(authID, fsm)
	}
}

func (w *Worker) ensureFSM(id string) {
	if _, ok := w.fsms[id]; ok {
		return
	}
	creds, client, err := w.loader(id)
	if err != nil {
		w.logs.append(LogEntry{
			Timestamp: w.nowFn(), Level: "warn", Scope: id,
			Message: "initial credential load failed: " + err.Error(),
		})
		return
	}
	fsm := NewAccountFSM(id, creds, w.cfg, client, w.nowFn, w.makeLogFn(id))
	// Seed nextWake=now so the very first loop tick steps this FSM. Without
	// this, a fresh FSM has zero nextWake and soonest() skips it forever.
	fsm.SetNextWake(w.nowFn())
	w.fsms[id] = fsm
}

func (w *Worker) makeLogFn(authID string) LogFn {
	return func(level, scope string, state State, msg, nextAction string, nextAt *time.Time, details map[string]any) {
		w.logs.append(LogEntry{
			Timestamp: w.nowFn(), Level: level, Scope: scope, State: state,
			Message: msg, NextAction: nextAction, NextAt: nextAt, Details: details,
		})
	}
}

func (w *Worker) soonest() (string, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var bestID string
	var best time.Time
	for id, fsm := range w.fsms {
		nw := fsm.NextWake()
		if nw.IsZero() {
			continue
		}
		if bestID == "" || nw.Before(best) {
			bestID = id
			best = nw
		}
	}
	return bestID, best
}

func (w *Worker) sleepOrStop(d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-w.stopCh:
	case <-timer.C:
	}
}

// advanceClock jumps the injected clock to t. This is a hook for tests using
// testAccelerate: when nowFn is backed by a *fakeWorkerClock (or similar
// mutable clock), advancing it lets the loop reach future wake times without
// real sleeping. The hook works by type-asserting a clock-advancer interface
// so production's time.Now is unaffected.
func (w *Worker) advanceClock(t time.Time) {
	type advancer interface{ AdvanceTo(time.Time) }
	// nowFn is a closure; if it captures a mutable clock, the clock may expose
	// an AdvanceTo method we can call. We detect via a package-level hook set
	// by tests (testClockAdvancer) to avoid reflection.
	if testClockAdvancer != nil {
		testClockAdvancer(t)
	}
}

// testClockAdvancer is set by tests to enable testAccelerate mode. Production
// leaves it nil. This avoids reflection on the nowFn closure.
var testClockAdvancer func(time.Time)
