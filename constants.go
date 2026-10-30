package main

import "time"

// OpenAI endpoint URLs — hardcoded per spec §2.1, never user-configurable.
// These full-URL constants are the authoritative documented contract; the
// OpenAI client also keeps *Path-suffixed constants for test BaseURL override.
const (
	usageEndpoint        = "https://chatgpt.com/backend-api/wham/usage"
	resetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	consumeEndpoint      = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

// codexUserAgent mirrors the value used by Cli-Proxy-API-Management-Center
// (spec §2.2). The codex_cli_rs/<version> prefix is the load-bearing part.
const codexUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

// Hardcoded scheduling/retry/log parameters (spec §7.2). None are user-tunable.
// resetRetryDelays must be a var because Go does not permit const slices;
// every other value here is a true const.
const (
	postResetVerifyDelay = 1 * time.Minute
	maxLogEntries        = 1000
	logRetention         = 7 * 24 * time.Hour
	// credentialRetryInterval is how soon an enabled account is retried after
	// its credentials fail to load (CPA auth file not ready at startup, token
	// mid-refresh, or operator re-login). Previously a failed initial load
	// left the account out of the FSM map forever, and a failure while running
	// waited a full refresh_interval (default 12h).
	credentialRetryInterval = 30 * time.Minute
	// wildcardResolveInterval throttles how often the worker re-queries the
	// host for the Codex account set while the all-accounts wildcard ("*") is
	// enabled. Without it the idle loop (100ms ticks when nothing is
	// scheduled) would hammer the host ABI.
	wildcardResolveInterval = 1 * time.Minute
)

var resetRetryDelays = []time.Duration{
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
}
