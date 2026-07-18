// Code under design exploration — NOT plugin production code.
// Deterministic event-driven simulator for the Codex Auto Reset state machine.
// Validates the ARMED-sleep-to-T design without touching the network.
package sim

import (
	"fmt"
	"sort"
	"time"
)

// Config holds the user-tunable parameters. Defaults match the design spec.
type Config struct {
	RefreshInterval    time.Duration // R: IDLE patrol cadence
	TriggerLeadTime    time.Duration // L: redeem this long before expiry
	PostResetDelay     time.Duration // hardcoded: verify delay after POST
}

func DefaultConfig() Config {
	return Config{
		RefreshInterval: 12 * time.Hour,
		TriggerLeadTime: 6 * time.Hour,
		PostResetDelay:  1 * time.Minute,
	}
}

// State is the per-account FSM state.
type State string

const (
	StateIDLE       State = "IDLE"
	StateARMED      State = "ARMED"
	StateCONFIRMING State = "CONFIRMING"
	StateRESETTING  State = "RESETTING"
	StateVERIFYING  State = "VERIFYING"
	StateDONE       State = "DONE"
)

// Credit models one reset credit row from GET /rate-limit-reset-credits.
type Credit struct {
	ID        string
	Status    string // "available" or other
	ExpiresAt time.Time
}

// Snapshot is the (credits, weekly-remaining) pair captured at key moments.
type Snapshot struct {
	AvailableCount int
	Credits        []Credit
	WeeklyPct      int // 0-100
}

// FakeOpenAIClient models the upstream. It records every call and lets tests
// script responses and side-effects (e.g. "consume removes credit X").
type FakeOpenAIClient struct {
	ListCalls      int
	UsageCalls     int
	ConsumedCalls  int
	ConsumedIDs    []string               // ordered list of credit_ids POSTed to /consume
	RedeemRequests []string               // ordered redeem_request_ids seen
	Credits        map[string]*Credit     // live credit store, keyed by id
	WeeklyPct      int                    // current weekly quota %
	ConsumeCode    string                 // code to return on POST: "reset"/"already_redeemed"/"no_credit"/"nothing_to_reset"
	WindowsReset   int                    // windows_reset value to return
	ListErr        error
	// FailFirstN, if > 0, makes the next N consume calls return this error
	// BEFORE applying the server-side deduction. Lets tests script transient
	// failures cleanly without fighting the OnConsumeReset timing.
	FailFirstN     int
	FailErr        error
	OnConsumeReset func(creditID string) // hook: simulate server-side deduction (only on code=="reset")
}

func (c *FakeOpenAIClient) list(now time.Time) Snapshot {
	c.ListCalls++
	credits := make([]Credit, 0, len(c.Credits))
	for _, cr := range c.Credits {
		// Local defensive filter (§2.3 step 1 in spec): exclude expired.
		if cr.ExpiresAt.Before(now) || cr.ExpiresAt.Equal(now) {
			continue
		}
		credits = append(credits, *cr)
	}
	sort.Slice(credits, func(i, j int) bool {
		return credits[i].ExpiresAt.Before(credits[j].ExpiresAt)
	})
	avail := 0
	for _, cr := range credits {
		if cr.Status == "available" {
			avail++
		}
	}
	return Snapshot{AvailableCount: avail, Credits: credits, WeeklyPct: c.WeeklyPct}
}

func (c *FakeOpenAIClient) usage() Snapshot {
	c.UsageCalls++
	avail := 0
	for _, cr := range c.Credits {
		if cr.Status == "available" {
			avail++
		}
	}
	return Snapshot{AvailableCount: avail, WeeklyPct: c.WeeklyPct}
}

func (c *FakeOpenAIClient) consume(redeemRequestID, creditID string) (code string, windowsReset int, err error) {
	c.ConsumedCalls++
	c.ConsumedIDs = append(c.ConsumedIDs, creditID)
	c.RedeemRequests = append(c.RedeemRequests, redeemRequestID)
	// Transient-failure injection takes precedence over the happy path so the
	// server-side deduction is NOT applied on a failed attempt.
	if c.FailFirstN > 0 && c.FailErr != nil {
		c.FailFirstN--
		return "", 0, c.FailErr
	}
	// Both "reset" and "already_redeemed" mean the credit was consumed server-side
	// (the latter by a prior replay with the same redeem_request_id). Apply the
	// side-effect hook in either case so verification sees the correct post-state.
	if c.OnConsumeReset != nil && (c.ConsumeCode == "reset" || c.ConsumeCode == "already_redeemed") {
		c.OnConsumeReset(creditID)
	}
	return c.ConsumeCode, c.WindowsReset, nil
}

// EventType for the audit trail.
type EventType string

const (
	EvStateChange EventType = "state"
	EvHTTPList    EventType = "http_list"
	EvHTTPUsage   EventType = "http_usage"
	EvHTTPConsume EventType = "http_consume"
	EvLog         EventType = "log"
)

// Event is one recorded step in the simulation.
type Event struct {
	Time    time.Time
	Type    EventType
	State   State
	Detail  string
	Next    *time.Time // scheduled next wake, if any
}

// Outcome is what assertions run against.
type Outcome struct {
	ResetAt        time.Time // when POST /consume succeeded (zero if never)
	ConsumedIDs    []string  // credits actually consumed
	NumConsumePOST int       // total POST /consume calls
	NumListCalls   int
	NumUsageCalls  int
	FinalState     State
	Events         []Event
	GaveUp         bool   // true if we abandoned without a successful reset
	GiveUpReason   string // why
}

// accountFSM is one account's state machine. Deterministic given inputs.
type accountFSM struct {
	cfg       Config
	credit    Credit // the single credit under management (status reflects upstream)
	client    *FakeOpenAIClient
	now       time.Time
	state     State
	redeemID  string
	targetID  string
	preSnap   Snapshot
	attempt   int
	nextRetry time.Time
	verifyDue time.Time
	out       *Outcome
}

func newFSM(cfg Config, client *FakeOpenAIClient, startNow time.Time, creditID string) *accountFSM {
	return &accountFSM{
		cfg:    cfg,
		client: client,
		now:    startNow,
		state:  StateIDLE,
		out:    &Outcome{FinalState: StateIDLE, Events: nil},
	}
}

func (f *accountFSM) recordState(next *time.Time, detail string) {
	f.out.Events = append(f.out.Events, Event{
		Time:   f.now,
		Type:   EvStateChange,
		State:  f.state,
		Detail: detail,
		Next:   next,
	})
}

// triggerTime returns T = E - L (the moment the trigger window opens).
func (f *accountFSM) triggerTime(credit Credit) time.Time {
	return credit.ExpiresAt.Add(-f.cfg.TriggerLeadTime)
}

// armThreshold returns M = T - R (the moment we enter ARMED).
func (f *accountFSM) armThreshold(credit Credit) time.Time {
	return f.triggerTime(credit).Add(-f.cfg.RefreshInterval)
}

// pickTarget applies §2.3: filter available, sort by expires_at asc, take first.
// Returns the chosen credit and whether any available credit exists.
func (f *accountFSM) pickTarget(snap Snapshot) (Credit, bool) {
	for _, cr := range snap.Credits {
		if cr.Status == "available" {
			return cr, true
		}
	}
	return Credit{}, false
}

// step executes one FSM tick at time f.now and returns the next scheduled wake.
// Returns zero time if the FSM has terminated.
func (f *accountFSM) step() time.Time {
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
		return time.Time{} // terminal this cycle; caller restarts IDLE
	}
	return time.Time{}
}

func (f *accountFSM) stepIDLE() time.Time {
	snap := f.client.list(f.now)
	f.out.NumListCalls = f.client.ListCalls
	f.out.Events = append(f.out.Events, Event{Time: f.now, Type: EvHTTPList, State: f.state, Detail: fmt.Sprintf("available=%d weekly=%d%%", snap.AvailableCount, snap.WeeklyPct)})

	target, ok := f.pickTarget(snap)
	if !ok {
		next := f.now.Add(f.cfg.RefreshInterval)
		f.recordState(&next, "no available credit")
		return next
	}

	M := f.armThreshold(target)
	T := f.triggerTime(target)
	if f.now.Before(M) {
		// Not yet near window.
		next := f.now.Add(f.cfg.RefreshInterval)
		f.recordState(&next, fmt.Sprintf("credit %s not near window (M=%s)", target.ID, M.Format("15:04:05")))
		return next
	}
	// now >= M -> ARMED. Wake at T (or immediately if now >= T).
	f.state = StateARMED
	wake := T
	if !f.now.Before(T) {
		wake = f.now // window already open; ARMED will proceed right away
	}
	f.recordState(&wake, fmt.Sprintf("credit %s armed (T=%s)", target.ID, T.Format("15:04:05")))
	return wake
}

func (f *accountFSM) stepARMED() time.Time {
	// ARMED just sleeps to T; the wake has already happened. Proceed to CONFIRMING.
	f.state = StateCONFIRMING
	f.recordState(nil, "armed wake -> confirming")
	return f.now
}

func (f *accountFSM) stepCONFIRMING() time.Time {
	snap := f.client.list(f.now)
	f.out.NumListCalls = f.client.ListCalls
	f.out.Events = append(f.out.Events, Event{Time: f.now, Type: EvHTTPList, State: f.state, Detail: fmt.Sprintf("confirm available=%d", snap.AvailableCount)})

	target, ok := f.pickTarget(snap)
	if !ok {
		f.state = StateDONE
		f.out.GaveUp = true
		f.out.GiveUpReason = "credit disappeared before reset"
		f.recordState(nil, "confirm: target gone, abandon")
		return time.Time{}
	}
	f.targetID = target.ID
	f.preSnap = snap
	f.redeemID = fmt.Sprintf("rrid-%d-%s", f.out.NumConsumePOST+1, target.ID)
	f.attempt = 0
	f.state = StateRESETTING
	f.recordState(nil, fmt.Sprintf("confirm ok target=%s pre weekly=%d%% -> resetting", target.ID, snap.WeeklyPct))
	return f.now
}

func (f *accountFSM) stepRESETTING() time.Time {
	code, wr, err := f.client.consume(f.redeemID, f.targetID)
	f.out.NumConsumePOST = len(f.client.ConsumedIDs)
	f.out.Events = append(f.out.Events, Event{Time: f.now, Type: EvHTTPConsume, State: f.state, Detail: fmt.Sprintf("POST redeem=%s credit=%s code=%q wr=%d err=%v", f.redeemID, f.targetID, code, wr, err)})
	if err != nil {
		// Retry with same redeem_request_id after backoff.
		f.attempt++
		if f.attempt > 5 {
			f.state = StateDONE
			f.out.GaveUp = true
			f.out.GiveUpReason = fmt.Sprintf("retries exhausted (last err=%v)", err)
			f.recordState(nil, "retries exhausted, abandon")
			return time.Time{}
		}
		backoff := backoffFor(f.attempt)
		f.nextRetry = f.now.Add(backoff)
		f.recordState(&f.nextRetry, fmt.Sprintf("consume err, retry %d in %v (same UUID)", f.attempt, backoff))
		return f.nextRetry
	}
	switch code {
	case "reset", "already_redeemed":
		f.state = StateVERIFYING
		f.verifyDue = f.now.Add(f.cfg.PostResetDelay)
		f.out.ResetAt = f.now
		f.recordState(&f.verifyDue, fmt.Sprintf("consume ok code=%s -> verify in %v", code, f.cfg.PostResetDelay))
		return f.verifyDue
	case "no_credit", "nothing_to_reset":
		f.state = StateDONE
		f.out.GaveUp = true
		f.out.GiveUpReason = fmt.Sprintf("consume returned %q", code)
		f.recordState(nil, fmt.Sprintf("consume %q, abandon", code))
		return time.Time{}
	}
	f.state = StateDONE
	f.out.GaveUp = true
	f.out.GiveUpReason = fmt.Sprintf("unknown consume code %q", code)
	f.recordState(nil, fmt.Sprintf("unknown code %q, abandon", code))
	return time.Time{}
}

func (f *accountFSM) stepVERIFYING() time.Time {
	// Re-confirm before verifying (§5.4 spirit): re-list, then judge.
	snap := f.client.list(f.now)
	f.out.NumListCalls = f.client.ListCalls
	usage := f.client.usage()
	f.out.NumUsageCalls = f.client.UsageCalls
	f.out.Events = append(f.out.Events, Event{Time: f.now, Type: EvHTTPList, State: f.state, Detail: fmt.Sprintf("verify list available=%d", snap.AvailableCount)})
	f.out.Events = append(f.out.Events, Event{Time: f.now, Type: EvHTTPUsage, State: f.state, Detail: fmt.Sprintf("verify usage weekly=%d%%", usage.WeeklyPct)})

	// Triple deduction check.
	targetGone := true
	for _, cr := range snap.Credits {
		if cr.ID == f.targetID && cr.Status == "available" {
			targetGone = false
			break
		}
	}
	countDown1 := snap.AvailableCount == f.preSnap.AvailableCount-1
	quotaRecovered := usage.WeeklyPct >= 100 || usage.WeeklyPct > f.preSnap.WeeklyPct

	f.out.ConsumedIDs = f.client.ConsumedIDs
	f.state = StateDONE
	switch {
	case targetGone && countDown1 && quotaRecovered:
		f.recordState(nil, fmt.Sprintf("verify OK target=%s gone, count %d->%d, weekly %d%%->%d%%", f.targetID, f.preSnap.AvailableCount, snap.AvailableCount, f.preSnap.WeeklyPct, usage.WeeklyPct))
	case targetGone && countDown1:
		f.recordState(nil, fmt.Sprintf("verify PARTIAL count ok but weekly %d%%->%d%% (delayed?)", f.preSnap.WeeklyPct, usage.WeeklyPct))
	default:
		f.out.GaveUp = true
		f.out.GiveUpReason = fmt.Sprintf("verification mismatch (targetGone=%v countDown1=%v quotaRecovered=%v)", targetGone, countDown1, quotaRecovered)
		f.recordState(nil, fmt.Sprintf("verify MISMATCH targetGone=%v countDown1=%v quotaUp=%v — halt", targetGone, countDown1, quotaRecovered))
	}
	return time.Time{}
}

func backoffFor(attempt int) time.Duration {
	delays := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		5 * time.Minute,
		10 * time.Minute,
		30 * time.Minute,
	}
	if attempt < 1 || attempt > len(delays) {
		return 30 * time.Minute
	}
	return delays[attempt-1]
}

// Run drives the FSM from startNow until either the cycle terminates (DONE)
// or maxSim time elapses. Returns the Outcome. The schedule returned by step()
// is honored; we advance now to that time, but we also cap advances so a bug
// causing a far-future wake can't hang the test.
func Run(cfg Config, client *FakeOpenAIClient, startNow time.Time, maxSim time.Duration) *Outcome {
	fsm := newFSM(cfg, client, startNow, "")
	deadline := startNow.Add(maxSim)
	steps := 0
	const maxSteps = 200
	for fsm.state != StateDONE && steps < maxSteps {
		steps++
		next := fsm.step()
		if next.IsZero() {
			break
		}
		if next.Before(fsm.now) || next.Equal(fsm.now) {
			// Same instant; keep stepping without advancing.
			continue
		}
		if next.After(deadline) {
			fsm.out.GaveUp = true
			fsm.out.GiveUpReason = fmt.Sprintf("simulation deadline reached at %s (state=%s)", deadline.Format("15:04:05"), fsm.state)
			break
		}
		fsm.now = next
	}
	fsm.out.FinalState = fsm.state
	return fsm.out
}
