package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// ConsumeCode is the snake_case `code` enum from POST /consume responses
// (spec §2.1). Values are locked by the upstream contract test.
type ConsumeCode string

const (
	ConsumeCodeReset           ConsumeCode = "reset"
	ConsumeCodeAlreadyRedeemed ConsumeCode = "already_redeemed"
	ConsumeCodeNoCredit        ConsumeCode = "no_credit"
	ConsumeCodeNothingToReset  ConsumeCode = "nothing_to_reset"
)

// ConsumeResponse mirrors POST /rate-limit-reset-credits/consume responses.
type ConsumeResponse struct {
	Code         ConsumeCode `json:"code"`
	WindowsReset int         `json:"windows_reset"`
}

// rawCredit captures only the fields we use from one row of the
// rate-limit-reset-credits response. Other returned fields (reset_type,
// granted_at, title, description, redeem_started_at, redeemed_at,
// profile_image_url, profile_user_id) are accepted but ignored per spec §2.1.
type rawCredit struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	ExpiresAt *string `json:"expires_at"`
}

type resetCreditsBody struct {
	Credits        []rawCredit `json:"credits"`
	AvailableCount int         `json:"available_count"`
	TotalEarned    int         `json:"total_earned_count"` // accepted, ignored
}

// parseResetCreditsResponse parses GET /rate-limit-reset-credits. It applies
// the §2.3 local filter+sort (status=="available", expires_at > now unless
// zero) so the returned Credits slice is already the production target list.
// AvailableCount comes straight from the server response body (authoritative).
func parseResetCreditsResponse(raw []byte, now time.Time) (Snapshot, error) {
	var body resetCreditsBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return Snapshot{}, fmt.Errorf("parse reset-credits response: %w", err)
	}
	credits := make([]Credit, 0, len(body.Credits))
	for _, rc := range body.Credits {
		c := Credit{ID: rc.ID, Status: rc.Status}
		if rc.ExpiresAt != nil && *rc.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, *rc.ExpiresAt); err == nil {
				c.ExpiresAt = t
			}
		}
		credits = append(credits, c)
	}
	credits = availableCreditsSorted(credits, now)
	return Snapshot{
		AvailableCount: body.AvailableCount,
		Credits:        credits,
		WeeklyPct:      -1, // not provided by this endpoint
		CapturedAt:     now,
	}, nil
}

type rateLimitWindow struct {
	Kind          string  `json:"kind"`
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int64   `json:"window_seconds"`
}

type usageBody struct {
	RateLimits []rateLimitWindow `json:"rate_limits"`
	// rate_limit_reset_credits is a summary form present on /wham/usage; the
	// detailed credit list comes from /wham/rate-limit-reset-credits instead.
	ResetCreditsSummary struct {
		AvailableCount int `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
}

// parseUsageResponse parses GET /wham/usage. WeeklyPct is the REMAINING weekly
// quota (100 - used_percent), rounded. If no weekly window is present the
// account is treated as full (100%).
func parseUsageResponse(raw []byte, now time.Time) (Snapshot, error) {
	var body usageBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return Snapshot{}, fmt.Errorf("parse usage response: %w", err)
	}
	weeklyPct := 100
	for _, w := range body.RateLimits {
		if w.Kind == "weekly" {
			used := int(w.UsedPercent + 0.5)
			weeklyPct = 100 - used
			if weeklyPct < 0 {
				weeklyPct = 0
			}
			break
		}
	}
	return Snapshot{
		AvailableCount: body.ResetCreditsSummary.AvailableCount,
		WeeklyPct:      weeklyPct,
		CapturedAt:     now,
	}, nil
}

// parseConsumeResponse parses POST /rate-limit-reset-credits/consume.
// windows_reset defaults to 0 when absent (upstream marks it omitempty).
func parseConsumeResponse(raw []byte) (ConsumeResponse, error) {
	var resp ConsumeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ConsumeResponse{}, fmt.Errorf("parse consume response: %w", err)
	}
	return resp, nil
}
