package main

import (
	"sort"
	"time"
)

// State is a per-account FSM state (spec §4.1).
type State string

const (
	StateIDLE       State = "IDLE"
	StateARMED      State = "ARMED"
	StateCONFIRMING State = "CONFIRMING"
	StateRESETTING  State = "RESETTING"
	StateVERIFYING  State = "VERIFYING"
	StateDONE       State = "DONE"
)

// Credit models one reset-credit row from GET /rate-limit-reset-credits
// (spec §2.1). A zero ExpiresAt means "never expires".
type Credit struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Snapshot is a (credits, weekly %) pair captured at key moments during a
// reset cycle: pre-reset confirmation, and post-reset verification.
type Snapshot struct {
	AvailableCount int       `json:"available_count"`
	Credits        []Credit  `json:"credits"`
	WeeklyPct      int       `json:"weekly_pct"`
	CapturedAt     time.Time `json:"captured_at"`
}

// ResetAttempt tracks one logical reset cycle (spec §5.1). A single
// RedeemRequestID is minted on entry to RESETTING and reused for every retry
// in that cycle, so OpenAI dedupes and never double-consumes.
type ResetAttempt struct {
	RedeemRequestID string    `json:"redeem_request_id"`
	TargetCreditID  string    `json:"target_credit_id"`
	PreSnapshot     Snapshot  `json:"pre_snapshot"`
	StartedAt       time.Time `json:"started_at"`
	Attempt         int       `json:"attempt"`
	NextRetryAt     time.Time `json:"next_retry_at"`
}

// LogEntry is one audit-trail record (spec §8.1). Every state transition and
// HTTP call appends one; each captures "what the last cycle did" plus
// "what the next cycle will do and when".
type LogEntry struct {
	Timestamp  time.Time      `json:"timestamp"`
	Level      string         `json:"level"`       // info / warn / error
	Scope      string         `json:"scope"`       // "system" or auth ID
	State      State          `json:"state"`       // current FSM state
	Message    string         `json:"message"`     // last cycle summary
	NextAction string         `json:"next_action"` // next cycle plan (optional)
	NextAt     *time.Time     `json:"next_at,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// availableCreditsSorted implements spec §2.3: filter to status=="available"
// AND expires_at > now (local defensive filter against server-side cleanup
// lag), then sort ascending by ExpiresAt with zero-expiry ("never") entries
// last. This is the production port of the filter+sort that the simulation
// validated in sim/engine.go's FakeOpenAIClient.list plus pickTarget.
func availableCreditsSorted(credits []Credit, now time.Time) []Credit {
	out := make([]Credit, 0, len(credits))
	for _, c := range credits {
		if c.Status != "available" {
			continue
		}
		if !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now) {
			continue // already expired; local filter
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ExpiresAt.IsZero() {
			return false // never-expiry sorts last
		}
		if out[j].ExpiresAt.IsZero() {
			return true
		}
		return out[i].ExpiresAt.Before(out[j].ExpiresAt)
	})
	return out
}
