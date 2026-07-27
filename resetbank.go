package main

import (
	"fmt"
	"strings"
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
	// Also fetch weekly % so the UI card can display it. The /rate-limit-
	// reset-credits endpoint doesn't return weekly %, so we need /usage.
	if usage, err := f.Client.GetUsage(f.Creds); err == nil {
		snap.WeeklyPct = usage.WeeklyPct
		f.lastSnapshot = snap
	}
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
			fmt.Sprintf("credit %s expires in %s; not near trigger window (will arm %s before expiry point)", shortID(target.ID), target.ExpiresAt.Sub(now).Round(time.Hour), f.Cfg.RefreshInterval),
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
		fmt.Sprintf("credit %s armed; will reset at %s (expiry in %s)", shortID(target.ID), T.Format("15:04:05"), target.ExpiresAt.Sub(now).Round(time.Minute)),
		"sleep to trigger time", &wake,
		map[string]any{"credit_id": target.ID, "T": T})
	return wake
}

func (f *AccountFSM) stepARMED() time.Time {
	// ARMED only sleeps to T; the wake has happened. Proceed to CONFIRMING.
	// Zero extra GETs in this state (revised design).
	f.state = StateCONFIRMING
	f.log("info", "trigger time reached, confirming before reset", "send reset request", nil, nil)
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
		fmt.Sprintf("confirmed target credit %s (pre: credits=%d, weekly=%d%%); sending reset request",
			shortID(target.ID), snap.AvailableCount, snap.WeeklyPct),
		"send reset request", nil,
		map[string]any{
			"credit_id":           target.ID,
			"pre_credits":         snap.AvailableCount,
			"pre_weekly_pct":      snap.WeeklyPct,
			"target_expires_at":   target.ExpiresAt,
			"redeem_request_id":   f.attempt.RedeemRequestID,
		})
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
			fmt.Sprintf("reset request accepted (code=%s, windows_reset=%d); will verify in %v",
				resp.Code, resp.WindowsReset, postResetVerifyDelay),
			fmt.Sprintf("verify in %v", postResetVerifyDelay), &f.verifyDue,
			map[string]any{
				"redeem_request_id": f.attempt.RedeemRequestID,
				"credit_id":         f.attempt.TargetCreditID,
				"consume_code":      resp.Code,
				"windows_reset":     resp.WindowsReset,
			})
		return f.verifyDue
	case decisionStop:
		f.state = StateDONE
		msg := fmt.Sprintf("reset stopped: server returned %s", resp.Code)
		if err != nil {
			msg = fmt.Sprintf("reset stopped: %v", err)
		}
		f.log("warn", msg, "abandon", nil, map[string]any{"redeem_request_id": f.attempt.RedeemRequestID})
		return time.Time{}
	case decisionRetry:
		f.attempt.Attempt++
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
			fmt.Sprintf("reset failed (attempt %d): %v; retrying with same idempotency key", f.attempt.Attempt, err),
			fmt.Sprintf("retry in %v", backoff), &f.attempt.NextRetryAt,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "attempt": f.attempt.Attempt})
		return f.attempt.NextRetryAt
	}
	return time.Time{}
}

func (f *AccountFSM) stepVERIFYING() time.Time {
	credits, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log("error", fmt.Sprintf("verification failed: %v", err), "abandon", nil,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "target_credit_id": f.attempt.TargetCreditID})
		return time.Time{}
	}
	usage, err := f.Client.GetUsage(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log("error", fmt.Sprintf("verification failed: %v", err), "abandon", nil,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "target_credit_id": f.attempt.TargetCreditID})
		return time.Time{}
	}
	// Update lastSnapshot with post-reset data so the UI shows the new state.
	credits.WeeklyPct = usage.WeeklyPct
	f.lastSnapshot = credits

	// Triple deduction check (spec §4.3 invariant 3).
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
	// Build detailed details map with full before/after snapshot.
	details := map[string]any{
		"credit_id":            f.attempt.TargetCreditID,
		"redeem_request_id":    f.attempt.RedeemRequestID,
		"pre_credits":          f.attempt.PreSnapshot.AvailableCount,
		"post_credits":         credits.AvailableCount,
		"pre_weekly_pct":       f.attempt.PreSnapshot.WeeklyPct,
		"post_weekly_pct":      usage.WeeklyPct,
		"target_gone":          targetGone,
		"count_down_1":         countDown1,
		"quota_recovered":      quotaUp,
	}

	switch {
	case targetGone && countDown1 && quotaUp:
		f.log("info",
			fmt.Sprintf("reset verified OK: credits %d→%d, weekly %d%%→%d%%, target %s consumed",
				f.attempt.PreSnapshot.AvailableCount, credits.AvailableCount,
				f.attempt.PreSnapshot.WeeklyPct, usage.WeeklyPct,
				shortID(f.attempt.TargetCreditID)),
			"cycle complete", nil, details)
	case targetGone && countDown1:
		f.log("warn",
			fmt.Sprintf("reset PARTIAL: credits %d→%d ok but weekly %d%%→%d%% (delayed recovery?)",
				f.attempt.PreSnapshot.AvailableCount, credits.AvailableCount,
				f.attempt.PreSnapshot.WeeklyPct, usage.WeeklyPct),
			"cycle complete", nil, details)
	default:
		f.log("error",
			fmt.Sprintf("reset MISMATCH: target_gone=%v count_down_1=%v quota_recovered=%v — halted, no compensating request",
				targetGone, countDown1, quotaUp),
			"abandon", nil, details)
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

// shortID truncates a long credit/redeem ID for readable log messages.
// "RateLimitResetCredit_d7087f83469c819182a87d5916512c9c" -> "d7087f83…"
func shortID(id string) string {
	const prefix = "RateLimitResetCredit_"
	s := strings.TrimPrefix(id, prefix)
	if len(s) > 10 {
		return s[:8] + "…"
	}
	return s
}
