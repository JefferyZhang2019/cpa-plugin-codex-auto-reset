package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ResetClient is the subset of OpenAIClient that the FSM needs. Defined here
// (next to the consumer) rather than in openai_client.go so the FSM is the
// dependency authority. Tests inject a fake; production injects *OpenAIClient.
type ResetClient interface {
	ListCredits(creds CodexCredentials) (Snapshot, error)
	GetUsage(creds CodexCredentials) (Snapshot, error)
	Consume(creds CodexCredentials, redeemRequestID, creditID string) (ConsumeResponse, error)
}

// LogFn is the FSM's logging callback. The worker wires it to the log ring.
type LogFn func(level, scope string, state State, msg, nextAction string, nextAt *time.Time, details map[string]any)

// AccountFSM is one account's state machine (spec §4). Deterministic given
// inputs; the simulation in sim/engine.go already validated the transitions.
//
// Concurrency: the worker goroutine drives Step(). External readers (worker's
// EachFSM, management handlers) may call State()/NextWake()/SetNextWake()/
// ForceState() from other goroutines; all field access is guarded by mu.
type AccountFSM struct {
	AuthID string
	Creds  CodexCredentials
	Cfg    Config
	Client ResetClient
	Now    func() time.Time
	Logf   LogFn

	mu        sync.Mutex
	state     State
	attempt   ResetAttempt
	nextWake  time.Time
	verifyDue time.Time
	// lastSnapshot is the most recent Snapshot observed by any state step
	// (IDLE patrol, CONFIRMING pre-reset, VERIFYING post-reset). The worker
	// mirrors it into AccountRuntime so the management UI can show live
	// credit counts, weekly %, and next expiry without an extra API call.
	lastSnapshot Snapshot
}

// NewAccountFSM builds an FSM in StateIDLE, ready for its first Step().
func NewAccountFSM(authID string, creds CodexCredentials, cfg Config, client ResetClient, now func() time.Time, logf LogFn) *AccountFSM {
	return &AccountFSM{
		AuthID: authID, Creds: creds, Cfg: cfg, Client: client, Now: now, Logf: logf,
		state: StateIDLE,
	}
}

// State returns the current FSM state (thread-safe).
func (f *AccountFSM) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// NextWake returns the most recently scheduled wake time (thread-safe).
func (f *AccountFSM) NextWake() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextWake
}

// LastSnapshot returns the most recent Snapshot observed by any state step.
// Thread-safe.
func (f *AccountFSM) LastSnapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSnapshot
}

// SetNextWake lets external callers (worker's TriggerCheck) override the
// scheduled wake. Thread-safe.
func (f *AccountFSM) SetNextWake(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextWake = t
}

// ForceState transitions the FSM to a target state (used by the worker's
// manual-reset ForceConfirm path). Thread-safe.
func (f *AccountFSM) ForceState(s State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

// Step executes one FSM tick at f.Now() and returns the next scheduled wake.
// Returns zero time when the cycle terminated (DONE). The behavior mirrors
// sim/engine.go's step() one-to-one; that simulation validated the design
// across 231 tests covering all (R, L) relations and edge cases.
//
// Step takes the FSM mutex for the whole tick; HTTP calls happen under lock,
// which is fine because the FSM is driven by a single worker goroutine (no
// other caller Step()s the same FSM).
func (f *AccountFSM) Step() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case StateIDLE:
		return f.stepIDLE()
	case StateARMED:
		return f.stepARMED()
	case StateCONFIRMING:
		return f.stepCONFIRMING()
	case StateRESETTING:
		return f.stepRESETTING()
	case StateVERIFYING:
		return f.stepVERIFYING()
	case StateDONE:
		return time.Time{}
	}
	return time.Time{}
}

func (f *AccountFSM) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// triggerTime returns T = E - L (the moment the trigger window opens).
func (f *AccountFSM) triggerTime(c Credit) time.Time { return c.ExpiresAt.Add(-f.Cfg.TriggerLeadTime) }

// armThreshold returns M = T - R (the moment we enter ARMED).
func (f *AccountFSM) armThreshold(c Credit) time.Time { return f.triggerTime(c).Add(-f.Cfg.RefreshInterval) }

func (f *AccountFSM) log(level, msg, nextAction string, nextAt *time.Time, details map[string]any) {
	if f.Logf != nil {
		f.Logf(level, f.AuthID, f.state, msg, nextAction, nextAt, details)
	}
}

func (f *AccountFSM) stepIDLE() time.Time {
	now := f.now()
	snap, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		next := now.Add(f.Cfg.RefreshInterval)
		f.nextWake = next
		f.log("warn", fmt.Sprintf("list failed: %v", err), "retry patrol", &next, nil)
		return next
	}
	f.lastSnapshot = snap // mirror into the UI-facing snapshot
	avail := availableCreditsSorted(snap.Credits, now)
	if len(avail) == 0 {
		next := now.Add(f.Cfg.RefreshInterval)
		f.nextWake = next
		f.log("info", "no available credits", "patrol", &next, nil)
		return next
	}
	target := avail[0]
	M := f.armThreshold(target)
	if now.Before(M) {
		next := now.Add(f.Cfg.RefreshInterval)
		f.nextWake = next
		f.log("info",
			fmt.Sprintf("credit %s not near trigger window (M=%s)", target.ID, M.Format(time.RFC3339)),
			"patrol", &next,
			map[string]any{"credit_id": target.ID, "expires_at": target.ExpiresAt})
		return next
	}
	// now >= M -> ARMED. Sleep to T (or now if T already passed: late-start,
	// spec §4.2.1). ARMED does NO extra GETs; it just transitions on wake.
	T := f.triggerTime(target)
	f.state = StateARMED
	wake := T
	if !now.Before(T) {
		wake = now
	}
	f.nextWake = wake
	f.log("warn",
		fmt.Sprintf("credit %s armed; trigger window T=%s", target.ID, T.Format(time.RFC3339)),
		"sleep to T then confirm", &wake,
		map[string]any{"credit_id": target.ID, "T": T})
	return wake
}

func (f *AccountFSM) stepARMED() time.Time {
	// ARMED only sleeps to T; the wake has happened. Proceed to CONFIRMING.
	// Zero extra GETs in this state (revised design).
	f.state = StateCONFIRMING
	f.log("info", "armed wake at T, confirming", "send POST /consume", nil, nil)
	return f.now() // re-enter immediately
}

func (f *AccountFSM) stepCONFIRMING() time.Time {
	now := f.now()
	snap, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log("error", fmt.Sprintf("confirm list failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	avail := availableCreditsSorted(snap.Credits, now)
	if len(avail) == 0 {
		f.state = StateDONE
		f.log("warn", "target credit vanished before reset", "abandon", nil, nil)
		return time.Time{}
	}
	// Fetch usage separately so PreSnapshot.WeeklyPct carries a real value.
	// The reset-credits endpoint does NOT return weekly % (parseResetCreditsResponse
	// sets WeeklyPct=-1); without this call, the triple-check's quota leg
	// (quotaUp) would be trivially true (any value > -1) and the safety
	// invariant would degrade to a two-leg check. Spec §4.2 mandates both calls.
	usage, err := f.Client.GetUsage(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log("error", fmt.Sprintf("confirm usage failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	snap.WeeklyPct = usage.WeeklyPct
	target := avail[0]
	f.attempt = ResetAttempt{
		RedeemRequestID: uuid.NewString(),
		TargetCreditID:  target.ID,
		PreSnapshot:     snap,
		StartedAt:       now,
		Attempt:         0,
	}
	f.state = StateRESETTING
	f.log("info",
		fmt.Sprintf("confirmed target=%s, pre weekly=%d%%, redeem_id=%s", target.ID, snap.WeeklyPct, f.attempt.RedeemRequestID),
		"send POST /consume", nil,
		map[string]any{"credit_id": target.ID})
	return f.now()
}

func (f *AccountFSM) stepRESETTING() time.Time {
	now := f.now()
	resp, err := f.Client.Consume(f.Creds, f.attempt.RedeemRequestID, f.attempt.TargetCreditID)
	switch classifyConsume(resp, err) {
	case decisionSuccess:
		f.state = StateVERIFYING
		f.verifyDue = now.Add(postResetVerifyDelay)
		f.nextWake = f.verifyDue
		f.log("info",
			fmt.Sprintf("consume ok code=%s windows_reset=%d", resp.Code, resp.WindowsReset),
			fmt.Sprintf("verify in %v", postResetVerifyDelay), &f.verifyDue,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "credit_id": f.attempt.TargetCreditID})
		return f.verifyDue
	case decisionStop:
		f.state = StateDONE
		msg := fmt.Sprintf("consume returned %s", resp.Code)
		if err != nil {
			msg = fmt.Sprintf("consume error (hard stop): %v", err)
		}
		f.log("warn", msg, "abandon", nil, map[string]any{"redeem_request_id": f.attempt.RedeemRequestID})
		return time.Time{}
	case decisionRetry:
		f.attempt.Attempt++
		// spec §5.2: at most 5 retries (so 6 total attempts including the
		// initial). The Nth retry uses resetRetryDelays[N-1]; we cap the
		// attempt count here so backoffForAttempt always indexes in range.
		if f.attempt.Attempt > len(resetRetryDelays) {
			f.state = StateDONE
			f.log("error",
				fmt.Sprintf("retries exhausted after %d attempts; last error: %v", f.attempt.Attempt-1, err),
				"abandon", nil,
				map[string]any{"redeem_request_id": f.attempt.RedeemRequestID})
			return time.Time{}
		}
		backoff := backoffForAttempt(f.attempt.Attempt)
		f.attempt.NextRetryAt = now.Add(backoff)
		f.nextWake = f.attempt.NextRetryAt
		f.log("warn",
			fmt.Sprintf("consume failed (attempt %d): %v; will retry with same UUID", f.attempt.Attempt, err),
			fmt.Sprintf("retry in %v (same redeem_request_id)", backoff), &f.attempt.NextRetryAt,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "attempt": f.attempt.Attempt})
		return f.attempt.NextRetryAt
	}
	return time.Time{}
}

func (f *AccountFSM) stepVERIFYING() time.Time {
	credits, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log("error", fmt.Sprintf("verify list failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	usage, err := f.Client.GetUsage(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log("error", fmt.Sprintf("verify usage failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	// Triple deduction check (spec §4.3 invariant 3). Mirrors sim/engine.go's
	// stepVERIFYING exactly: target gone, available count dropped by exactly 1,
	// and weekly quota recovered (>=100 OR strictly greater than pre-snapshot).
	targetGone := true
	for _, c := range credits.Credits {
		if c.ID == f.attempt.TargetCreditID && c.Status == "available" {
			targetGone = false
			break
		}
	}
	countDown1 := credits.AvailableCount == f.attempt.PreSnapshot.AvailableCount-1
	quotaUp := usage.WeeklyPct >= 100 || usage.WeeklyPct > f.attempt.PreSnapshot.WeeklyPct

	f.state = StateDONE
	switch {
	case targetGone && countDown1 && quotaUp:
		f.log("info",
			fmt.Sprintf("verify OK: count %d->%d, weekly %d%%->%d%%", f.attempt.PreSnapshot.AvailableCount, credits.AvailableCount, f.attempt.PreSnapshot.WeeklyPct, usage.WeeklyPct),
			"cycle complete", nil, map[string]any{"credit_id": f.attempt.TargetCreditID})
	case targetGone && countDown1:
		f.log("warn",
			fmt.Sprintf("verify PARTIAL: count ok but weekly %d%%->%d%% (delayed reset?)", f.attempt.PreSnapshot.WeeklyPct, usage.WeeklyPct),
			"cycle complete", nil, nil)
	default:
		f.log("error",
			fmt.Sprintf("verify MISMATCH: targetGone=%v countDown1=%v quotaUp=%v — halting, no compensating request", targetGone, countDown1, quotaUp),
			"abandon", nil,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "target_credit_id": f.attempt.TargetCreditID})
	}
	return time.Time{}
}

// Reset returns a DONE FSM to IDLE for the next patrol cycle. Called by the
// worker after a cycle completes. Thread-safe.
func (f *AccountFSM) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = StateIDLE
	f.attempt = ResetAttempt{}
	f.verifyDue = time.Time{}
	f.nextWake = time.Time{}
}
