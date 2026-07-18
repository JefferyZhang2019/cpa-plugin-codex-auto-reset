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

// parseUsageResponse parses GET /wham/usage using flexible map lookups.
// The real wire shape (confirmed from live data) is:
//   "rate_limit": {
//     "primary_window": {"limit_window_seconds": 604800, "used_percent": 9},
//     "secondary_window": null
//   }
// primary_window can be the WEEKLY window (604800s) when there's no
// secondary. We identify the weekly window by limit_window_seconds ≈ 604800,
// NOT by position (primary/secondary). code_review_rate_limit may also be
// present in some builds; we check both paths.
func parseUsageResponse(raw []byte, now time.Time) (Snapshot, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Snapshot{}, fmt.Errorf("parse usage response: %w", err)
	}

	availableCount := 0
	if credits, ok := getMapAny(doc, "rate_limit_reset_credits", "rateLimitResetCredits"); ok {
		if c, ok := getIntAny(credits, "available_count", "availableCount"); ok {
			availableCount = c
		}
	}

	// Scan all rate-limit paths for a window whose limit_window_seconds ≈ 604800
	// (weekly). The window may be primary or secondary — we check both and
	// pick the weekly one by duration, not position.
	weeklyUsed := -1.0
	for _, rlKey := range []string{"rate_limit", "rateLimit", "code_review_rate_limit", "codeReviewRateLimit"} {
		rl, ok := getMapAny(doc, rlKey)
		if !ok {
			continue
		}
		for _, winKey := range []string{"primary_window", "primaryWindow", "secondary_window", "secondaryWindow"} {
			win, ok := getMapAny(rl, winKey)
			if !ok {
				continue
			}
			secs, hasSecs := getIntAny(win, "limit_window_seconds", "limitWindowSeconds")
			if !hasSecs {
				continue
			}
			// Weekly = ~604800 (7 * 86400). Accept ±1 hour tolerance.
			if secs >= 600000 && secs <= 610000 {
				if used, ok := getFloatAny(win, "used_percent", "usedPercent"); ok {
					weeklyUsed = used
					break
				}
			}
		}
		if weeklyUsed >= 0 {
			break
		}
	}

	weeklyPct := 100
	if weeklyUsed >= 0 {
		weeklyPct = 100 - int(weeklyUsed+0.5)
		if weeklyPct < 0 {
			weeklyPct = 0
		}
	}

	return Snapshot{
		AvailableCount: availableCount,
		WeeklyPct:      weeklyPct,
		CapturedAt:     now,
	}, nil
}

// --- flexible map helpers (mirror codex-quota-scheduler's getMap/getInt/getFloat64) ---

func getMapAny(root map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if v, ok := root[key]; ok {
			if m, ok := v.(map[string]any); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func getIntAny(root map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if v, ok := root[key]; ok {
			switch n := v.(type) {
			case float64:
				return int(n), true
			case int:
				return n, true
			}
		}
	}
	return 0, false
}

func getFloatAny(root map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if v, ok := root[key]; ok {
			if n, ok := v.(float64); ok {
				return n, true
			}
		}
	}
	return 0, false
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
