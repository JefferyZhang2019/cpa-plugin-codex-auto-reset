package main

import "time"

// retryDecision is what the FSM should do after a consume attempt (spec §5.3).
type retryDecision int

const (
	// decisionSuccess: code=reset or already_redeemed -> proceed to VERIFYING.
	decisionSuccess retryDecision = iota
	// decisionRetry: transient failure (5xx, network, timeout) -> retry with the
	// SAME redeem_request_id so the server dedupes.
	decisionRetry
	// decisionStop: terminal failure (no_credit, nothing_to_reset, 4xx, unknown
	// code) -> DONE without retrying. 4xx must hard-stop to avoid 48 minutes of
	// pointless hammering with a known-bad token.
	decisionStop
)

// backoffForAttempt returns the delay before the Nth retry (1-based). Attempts
// past the end of resetRetryDelays clamp to the last (longest) value, matching
// spec §5.2's "at most 5 retries" intent once the FSM caps the attempt count.
func backoffForAttempt(attempt int) time.Duration {
	if attempt < 1 || attempt > len(resetRetryDelays) {
		if len(resetRetryDelays) == 0 {
			return 0
		}
		return resetRetryDelays[len(resetRetryDelays)-1]
	}
	return resetRetryDelays[attempt-1]
}

// classifyConsume maps a consume response (or error) to the FSM's next action.
// Decision matrix (spec §5.3, tightened by code review):
//   - err == nil, code in {reset, already_redeemed}     -> Success
//   - err == nil, code in {no_credit, nothing_to_reset} -> Stop
//   - err == nil, unknown code                          -> Stop (don't hammer)
//   - err is 4xx HTTPStatusError                        -> Stop (bad token/request)
//   - err is 5xx / network / timeout / anything else    -> Retry (transient)
func classifyConsume(resp ConsumeResponse, err error) retryDecision {
	if err != nil {
		if IsClientError(err) {
			return decisionStop
		}
		return decisionRetry
	}
	switch resp.Code {
	case ConsumeCodeReset, ConsumeCodeAlreadyRedeemed:
		return decisionSuccess
	case ConsumeCodeNoCredit, ConsumeCodeNothingToReset:
		return decisionStop
	default:
		return decisionStop
	}
}
