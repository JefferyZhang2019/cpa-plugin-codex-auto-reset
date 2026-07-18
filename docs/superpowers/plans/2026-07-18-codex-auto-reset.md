# Codex Auto Reset Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a CPA dynamic-library plugin that automatically redeems the soonest-expiring Codex "Reset Bank" credit on each enabled account before it expires.

**Architecture:** Single Go c-shared library loaded by CPA. Three layers — pure OpenAI HTTP client (no CPA awareness), Management layer (FSM engine + scheduling + persistence + routes), and Resource layer (static bilingual HTML). A single background worker goroutine drives one FSM per enabled account; ARMED state sleeps precisely to the trigger time `T = expiry − lead_time` with zero extra polling.

**Tech Stack:** Go 1.26, CGO (`-buildmode=c-shared`), `github.com/router-for-me/CLIProxyAPI/v7` SDK (`pluginabi` + `pluginapi`), `gopkg.in/yaml.v3`, `net/http/httptest` for tests.

**Spec:** `docs/superpowers/specs/2026-07-18-codex-auto-reset-design.md` — read it before starting. The spec's §2 wire-format contract and §4 state machine are authoritative; this plan implements them.

**Reference plugin:** `../cpa-plugin-codex-quota-scheduler/` — copy ABI boilerplate, build/release, and bilingual-UI patterns from it. Do not copy its scheduler/quota business logic; this plugin is independent.

**Module path:** `github.com/jeffery/cpa-plugin-codex-auto-reset` (matches the directory name; mirror the scheduler's `github.com/jeffery/codex-quota-scheduler` style).

---

## File Structure

Each file has one responsibility. Smaller files are intentional — the FSM logic in particular stays isolated so the simulation harness (`sim/`) and the production `resetbank.go` can share reasoning.

| File | Responsibility |
|---|---|
| `go.mod` | Module declaration; deps: CLIProxyAPI v7, yaml.v3 |
| `main.go` | C ABI entry (init/Call/Free/Shutdown), host-callback wiring, exports |
| `constants.go` | Hardcoded endpoint URLs, headers, retry delays, intervals (§7.2) |
| `auth.go` | `CodexCredentials` extraction + JWT `chatgpt_account_id` parse (copy from scheduler) |
| `config.go` | 3 user-configurable fields, YAML parse, normalize, defaults (§7.1) |
| `models.go` | `State`, `Credit`, `Snapshot`, `ResetAttempt`, `Outcome`, `LogEntry`, FSM-state strings |
| `openai_client.go` | Pure HTTP: `ListCredits`, `GetUsage`, `Consume` — byte-exact contract |
| `quota.go` | Parse responses; filter+sort credits per §2.3 (local `expires_at > now`) |
| `resetbank.go` | Per-account FSM engine (the 6-state machine from §4) |
| `retry.go` | Backoff sequence + retry-trigger classification |
| `worker.go` | Single goroutine: per-account time wheel, credential sync, manual triggers |
| `state.go` | Disk persistence (settings + per-account FSM state + log ring) |
| `log.go` | Log entry construction, ring buffer, retention prune |
| `management.go` | Management API routes + resource HTML + bilingual strings |
| `*_test.go` | One per source file |
| `Makefile`, `build.ps1` | Cross-compile (copy from scheduler, rename) |
| `.github/workflows/build.yml` | Release workflow (copy from scheduler, rename) |
| `.github/scripts/package-release.go` | Zip+checksum helper (copy from scheduler) |
| `README.md` | User-facing docs + §10.6 manual checklist |

**Out of scope for this plan:** the `sim/` directory already exists as design validation; it is NOT part of the plugin build (separate go.mod). Leave it alone.

---

## Task 0: Project skeleton and build tooling

**Files:**
- Create: `go.mod`
- Create: `Makefile`
- Create: `build.ps1`
- Create: `.gitignore`

- [ ] **Step 1: Create `go.mod`**

```
module github.com/jeffery/cpa-plugin-codex-auto-reset

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.2.42
	gopkg.in/yaml.v3 v3.0.1
)
```

- [ ] **Step 2: Create `.gitignore`**

Copy `../cpa-plugin-codex-quota-scheduler/.gitignore` verbatim.

- [ ] **Step 3: Create `Makefile`**

Copy `../cpa-plugin-codex-quota-scheduler/Makefile` and replace every occurrence of `codex-quota-scheduler` with `codex-auto-reset`. Default `VERSION ?= 0.1.0`.

- [ ] **Step 4: Create `build.ps1`**

Copy `../cpa-plugin-codex-quota-scheduler/build.ps1` and replace every `codex-quota-scheduler` with `codex-auto-reset`.

- [ ] **Step 5: Verify deps resolve**

Run: `go mod download`
Expected: no error. If `v7.2.42` is unavailable, run `go get github.com/router-for-me/CLIProxyAPI/v7@latest` and pin the resulting version in `go.mod`.

- [ ] **Step 6: Commit**

```bash
git init 2>/dev/null || true
git add go.mod Makefile build.ps1 .gitignore docs/ sim/
git commit -m "chore: scaffold codex-auto-reset plugin module and build tooling"
```

---

## Task 1: Constants — endpoints, headers, intervals

**Files:**
- Create: `constants.go`
- Test: `constants_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"
)

func TestConstants_MatchSpec(t *testing.T) {
	if usageEndpoint != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("usageEndpoint = %q", usageEndpoint)
	}
	if resetCreditsEndpoint != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
		t.Fatalf("resetCreditsEndpoint = %q", resetCreditsEndpoint)
	}
	if consumeEndpoint != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume" {
		t.Fatalf("consumeEndpoint = %q", consumeEndpoint)
	}
	if codexUserAgent != "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal" {
		t.Fatalf("codexUserAgent = %q", codexUserAgent)
	}
	if postResetVerifyDelay != 1*time.Minute {
		t.Fatalf("postResetVerifyDelay = %v", postResetVerifyDelay)
	}
	want := []time.Duration{1 * time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	if len(resetRetryDelays) != len(want) {
		t.Fatalf("resetRetryDelays len = %d", len(resetRetryDelays))
	}
	for i := range want {
		if resetRetryDelays[i] != want[i] {
			t.Fatalf("resetRetryDelays[%d] = %v, want %v", i, resetRetryDelays[i], want[i])
		}
	}
	if maxLogEntries != 200 {
		t.Fatalf("maxLogEntries = %d", maxLogEntries)
	}
	if logRetention != 24*time.Hour {
		t.Fatalf("logRetention = %v", logRetention)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestConstants_MatchSpec ./...`
Expected: FAIL — undefined identifiers.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import "time"

// OpenAI endpoint URLs — hardcoded per spec §2.1, never user-configurable.
const (
	usageEndpoint        = "https://chatgpt.com/backend-api/wham/usage"
	resetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	consumeEndpoint      = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

// User-Agent mirrors the value used by Cli-Proxy-API-Management-Center (spec §2.2).
const codexUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

// Hardcoded scheduling/retry/log parameters (spec §7.2).
const (
	postResetVerifyDelay = 1 * time.Minute
	maxLogEntries        = 200
)

var (
	logRetention     = 24 * time.Hour
	resetRetryDelays = []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		5 * time.Minute,
		10 * time.Minute,
		30 * time.Minute,
	}
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestConstants_MatchSpec ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add constants.go constants_test.go
git commit -m "feat: add hardcoded endpoint and tuning constants"
```

---

## Task 2: Auth — Codex credentials extraction

**Files:**
- Create: `auth.go`
- Test: `auth_test.go`

This is a near-verbatim copy of `../cpa-plugin-codex-quota-scheduler/auth.go` (the scheduler already solved JWT parsing). Keep function and type names identical so future cross-plugin refactors are easy.

- [ ] **Step 1: Copy `auth.go` from the scheduler**

Copy `../cpa-plugin-codex-quota-scheduler/auth.go` to `./auth.go`. No changes needed — the file has no scheduler-specific code.

- [ ] **Step 2: Copy `auth_test.go` from the scheduler**

Copy `../cpa-plugin-codex-quota-scheduler/auth_test.go` to `./auth_test.go`.

- [ ] **Step 3: Run tests**

Run: `go test -run TestExtract ./...`
Expected: all auth tests PASS. If any fail because the scheduler test references symbols not yet copied, delete only those subtests and note them in the commit message.

- [ ] **Step 4: Commit**

```bash
git add auth.go auth_test.go
git commit -m "feat: add Codex credential extraction (from scheduler)"
```

---

## Task 3: Models — state machine types

**Files:**
- Create: `models.go`
- Test: `models_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"
)

func TestCreditSortOrder(t *testing.T) {
	t1 := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	t2 := t1.Add(48 * time.Hour)
	in := []Credit{
		{ID: "later", Status: "available", ExpiresAt: t2},
		{ID: "never", Status: "available", ExpiresAt: time.Time{}}, // zero = never expires
		{ID: "sooner", Status: "available", ExpiresAt: t1},
		{ID: "used", Status: "redeemed", ExpiresAt: t1.Add(-time.Hour)},
	}
	got := availableCreditsSorted(in, t1.Add(-time.Hour))
	if len(got) != 3 {
		t.Fatalf("got %d available, want 3: %+v", len(got), got)
	}
	if got[0].ID != "sooner" || got[1].ID != "later" || got[2].ID != "never" {
		t.Fatalf("order = %s %s %s; never must sort last", got[0].ID, got[1].ID, got[2].ID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCreditSortOrder ./...`
Expected: FAIL — undefined `Credit`, `availableCreditsSorted`.

- [ ] **Step 3: Write minimal implementation**

```go
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

// Credit models one reset-credit row (spec §2.1). Zero ExpiresAt = never expires.
type Credit struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Snapshot is a (credits, weekly %) pair captured at key moments.
type Snapshot struct {
	AvailableCount int       `json:"available_count"`
	Credits        []Credit  `json:"credits"`
	WeeklyPct      int       `json:"weekly_pct"`
	CapturedAt     time.Time `json:"captured_at"`
}

// ResetAttempt tracks one logical reset cycle (spec §5.1).
type ResetAttempt struct {
	RedeemRequestID string    `json:"redeem_request_id"`
	TargetCreditID  string    `json:"target_credit_id"`
	PreSnapshot     Snapshot  `json:"pre_snapshot"`
	StartedAt       time.Time `json:"started_at"`
	Attempt         int       `json:"attempt"`
	NextRetryAt     time.Time `json:"next_retry_at"`
}

// availableCreditsSorted implements spec §2.3: filter status=="available" AND
// expires_at > now (local defensive filter), sort ascending with zero-expiry last.
func availableCreditsSorted(credits []Credit, now time.Time) []Credit {
	out := make([]Credit, 0, len(credits))
	for _, c := range credits {
		if c.Status != "available" {
			continue
		}
		if !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now) {
			continue // already expired; local filter (spec §2.3 step 1)
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Zero (never-expires) sorts last.
		if out[i].ExpiresAt.IsZero() {
			return false
		}
		if out[j].ExpiresAt.IsZero() {
			return true
		}
		return out[i].ExpiresAt.Before(out[j].ExpiresAt)
	})
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestCreditSortOrder ./...`
Expected: PASS.

- [ ] **Step 5: Add LogEntry type and test its JSON round-trip**

Append to `models_test.go`:

```go
func TestLogEntryJSONRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	entry := LogEntry{
		Timestamp: ts, Level: "info", Scope: "acct1", State: StateARMED,
		Message: "armed", NextAction: "sleep to T",
		NextAt: &ts, Details: map[string]any{"credit_id": "c1"},
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back LogEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Scope != "acct1" || back.State != StateARMED || back.Details["credit_id"] != "c1" {
		t.Fatalf("round-trip lost data: %+v", back)
	}
}
```

Add to `models.go`:

```go
import "encoding/json"

// LogEntry is one audit-trail record (spec §8.1).
type LogEntry struct {
	Timestamp  time.Time         `json:"timestamp"`
	Level      string            `json:"level"`       // info / warn / error
	Scope      string            `json:"scope"`       // "system" or auth ID
	State      State             `json:"state"`       // current FSM state
	Message    string            `json:"message"`     // last cycle summary
	NextAction string            `json:"next_action"` // next cycle plan (optional)
	NextAt     *time.Time        `json:"next_at,omitempty"`
	Details    map[string]any    `json:"details,omitempty"`
}

// MarshalJSON ensures empty LogEntry marshals as {} not null.
func (l LogEntry) MarshalJSON() ([]byte, error) {
	type alias LogEntry
	return json.Marshal(alias(l))
}
```

(Note: merge the two `import` blocks into one in the final file.)

- [ ] **Step 6: Run all model tests**

Run: `go test -run 'TestCreditSortOrder|TestLogEntry' ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add models.go models_test.go
git commit -m "feat: add FSM state, credit, snapshot, log-entry models"
```

---

## Task 4: Quota parsing — OpenAI response → Snapshot

**Files:**
- Create: `quota.go`
- Test: `quota_test.go`

Parses the two GET responses into `Snapshot`. Uses the exact fixture from the official CLI's contract test (spec §2.1).

- [ ] **Step 1: Write the failing test using the upstream contract fixture**

```go
package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseResetCreditsResponse_UpstreamContractFixture(t *testing.T) {
	raw := []byte(`{
	  "credits": [
	    {"id":"credit-1","reset_type":"codex_rate_limits","status":"available",
	     "granted_at":"2026-06-17T00:00:00Z","expires_at":"2026-07-17T00:00:00Z",
	     "title":"Full reset","description":"Ready"},
	    {"id":"credit-2","reset_type":"codex_rate_limits","status":"available",
	     "granted_at":"2026-06-18T00:00:00Z","expires_at":null}
	  ],
	  "available_count": 2,
	  "total_earned_count": 4
	}`)
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	snap, err := parseResetCreditsResponse(raw, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.AvailableCount != 2 {
		t.Fatalf("available_count = %d", snap.AvailableCount)
	}
	if len(snap.Credits) != 2 {
		t.Fatalf("credits len = %d", len(snap.Credits))
	}
	// credit-2 has null expires_at — must be present and zero-valued.
	if snap.Credits[1].ID != "credit-2" || !snap.Credits[1].ExpiresAt.IsZero() {
		t.Fatalf("credit-2 parse wrong: %+v", snap.Credits[1])
	}
}

func TestParseUsageResponse_WeeklyPercent(t *testing.T) {
	raw := []byte(`{
	  "plan_type":"plus",
	  "rate_limit_reset_credits":{"available_count":1},
	  "rate_limits":[
	    {"kind":"weekly","window_seconds":604800,"used_percent":15,"reset_at":"2026-07-25T00:00:00Z"}
	  ]
	}`)
	snap, err := parseUsageResponse(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// weekly used 15% => remaining 85
	if snap.WeeklyPct != 85 {
		t.Fatalf("WeeklyPct = %d, want 85", snap.WeeklyPct)
	}
	if snap.AvailableCount != 1 {
		t.Fatalf("available_count = %d", snap.AvailableCount)
	}
}

func TestParseConsumeResponse_Codes(t *testing.T) {
	cases := []struct{ code string; want ConsumeCode }{
		{`"reset"`, ConsumeCodeReset},
		{`"already_redeemed"`, ConsumeCodeAlreadyRedeemed},
		{`"no_credit"`, ConsumeCodeNoCredit},
		{`"nothing_to_reset"`, ConsumeCodeNothingToReset},
	}
	for _, tc := range cases {
		raw := []byte(`{"code":` + tc.code + `,"windows_reset":2}`)
		resp, err := parseConsumeResponse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.code, err)
		}
		if resp.Code != tc.want {
			t.Fatalf("code %s => %v, want %v", tc.code, resp.Code, tc.want)
		}
		if resp.WindowsReset != 2 {
			t.Fatalf("windows_reset = %d", resp.WindowsReset)
		}
	}
}

// Ensure unused helper compiles.
var _ = json.Marshal
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestParseResetCredits|TestParseUsage|TestParseConsume' ./...`
Expected: FAIL — undefined parsers and `ConsumeCode`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// ConsumeCode is the snake_case `code` enum from POST /consume (spec §2.1).
type ConsumeCode string

const (
	ConsumeCodeReset          ConsumeCode = "reset"
	ConsumeCodeAlreadyRedeemed ConsumeCode = "already_redeemed"
	ConsumeCodeNoCredit       ConsumeCode = "no_credit"
	ConsumeCodeNothingToReset ConsumeCode = "nothing_to_reset"
)

// ConsumeResponse mirrors POST /consume response.
type ConsumeResponse struct {
	Code         ConsumeCode `json:"code"`
	WindowsReset int         `json:"windows_reset"`
}

type rawCredit struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	ExpiresAt *string `json:"expires_at"`
	// Other fields (reset_type, granted_at, title, description, redeem_*,
	// profile_*) are returned by the server but ignored — spec §2.1.
}

type resetCreditsBody struct {
	Credits         []rawCredit `json:"credits"`
	AvailableCount  int         `json:"available_count"`
	TotalEarned     int         `json:"total_earned_count"` // ignored
}

func parseResetCreditsResponse(raw []byte, now time.Time) (Snapshot, error) {
	var body resetCreditsBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return Snapshot{}, fmt.Errorf("parse reset-credits response: %w", err)
	}
	credits := make([]Credit, 0, len(body.Credits))
	for _, rc := range body.Credits {
		c := Credit{ID: rc.ID, Status: rc.Status}
		if rc.ExpiresAt != nil && *rc.ExpiresAt != "" {
			t, err := time.Parse(time.RFC3339, *rc.ExpiresAt)
			if err == nil {
				c.ExpiresAt = t
			}
		}
		credits = append(credits, c)
	}
	// Apply local filter+sort (spec §2.3).
	credits = availableCreditsSorted(credits, now)
	avail := 0
	for _, c := range credits {
		if c.Status == "available" {
			avail++
		}
	}
	return Snapshot{
		AvailableCount: body.AvailableCount,
		Credits:        credits,
		WeeklyPct:      -1, // not provided by this endpoint
		CapturedAt:     now,
	}, nil
	_ = avail
}

type rateLimitWindow struct {
	Kind         string  `json:"kind"`
	UsedPercent  float64 `json:"used_percent"`
	WindowSeconds int64  `json:"window_seconds"`
}

type usageBody struct {
	RateLimits []rateLimitWindow `json:"rate_limits"`
	// rate_limit_reset_credits summary is available here too, but we prefer the
	// detailed /rate-limit-reset-credits endpoint for credit rows.
	ResetCreditsSummary struct {
		AvailableCount int `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
}

func parseUsageResponse(raw []byte, now time.Time) (Snapshot, error) {
	var body usageBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return Snapshot{}, fmt.Errorf("parse usage response: %w", err)
	}
	weeklyPct := 100 // default: no weekly window means full
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

func parseConsumeResponse(raw []byte) (ConsumeResponse, error) {
	var resp ConsumeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ConsumeResponse{}, fmt.Errorf("parse consume response: %w", err)
	}
	return resp, nil
}
```

(Remove the stray `_ = avail` line before committing — it was a placeholder; `avail` is intentionally unused since `Snapshot.AvailableCount` comes from the body. Either delete the unused `avail` block or keep `AvailableCount` computed from the filtered list. Decision: **use the server-reported `body.AvailableCount`**, so delete the local `avail` loop.)

- [ ] **Step 4: Clean up the unused `avail` computation**

In `parseResetCreditsResponse`, delete the `avail := 0; for ...` loop and the `_ = avail` line. Keep `body.AvailableCount` as the source of truth (matches upstream semantics where `available_count` is authoritative).

- [ ] **Step 5: Run tests**

Run: `go test -run 'TestParseResetCredits|TestParseUsage|TestParseConsume' ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add quota.go quota_test.go
git commit -m "feat: parse OpenAI reset-credits, usage, and consume responses"
```

---

## Task 5: OpenAI client — HTTP layer with byte-exact wire format

**Files:**
- Create: `openai_client.go`
- Test: `openai_client_test.go`

This is the most safety-critical file. Tests must assert the exact URL, headers, and body bytes (spec §2.1). Use `net/http/httptest`.

- [ ] **Step 1: Write the failing test asserting wire format**

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.Handler) (*OpenAIClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &OpenAIClient{
		BaseURL: srv.URL, // overrides hardcoded endpoints for testing
		HTTP:    srv.Client(),
	}, srv
}

func TestListCredits_WireFormat(t *testing.T) {
	var gotMethod, gotURL, gotAuth, gotAcct, gotUA string
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotURL = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("Chatgpt-Account-Id")
		gotUA = r.Header.Get("User-Agent")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"credits":[],"available_count":0}`))
	}))
	snap, err := cli.ListCredits(CodexCredentials{AccessToken: "tok", ChatGPTAccountID: "acct-1"})
	if err != nil {
		t.Fatalf("ListCredits: %v", err)
	}
	if snap.AvailableCount != 0 {
		t.Fatalf("snap = %+v", snap)
	}
	if gotMethod != "GET" || gotURL != "/backend-api/wham/rate-limit-reset-credits" {
		t.Fatalf("method/path = %s %s", gotMethod, gotURL)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotAcct != "acct-1" {
		t.Fatalf("account = %q", gotAcct)
	}
	if !strings.HasPrefix(gotUA, "codex_cli_rs/") {
		t.Fatalf("UA = %q", gotUA)
	}
}

func TestConsume_WireFormat_TargetedBody(t *testing.T) {
	var gotBody map[string]any
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"code":"reset","windows_reset":2}`))
	}))
	resp, err := cli.Consume(CodexCredentials{AccessToken: "t", ChatGPTAccountID: "a"}, "rrid-123", "credit-456")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if resp.Code != ConsumeCodeReset || resp.WindowsReset != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	// Byte-exact body assertion.
	if gotBody["redeem_request_id"] != "rrid-123" {
		t.Fatalf("redeem_request_id = %v", gotBody["redeem_request_id"])
	}
	if gotBody["credit_id"] != "credit-456" {
		t.Fatalf("credit_id = %v", gotBody["credit_id"])
	}
	if len(gotBody) != 2 {
		t.Fatalf("body has extra fields: %+v", gotBody)
	}
}

func TestConsume_HTTPError(t *testing.T) {
	cli, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream down`))
	}))
	_, err := cli.Consume(CodexCredentials{AccessToken: "t"}, "rrid", "cid")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected 502 error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestListCredits|TestConsume' ./...`
Expected: FAIL — undefined `OpenAIClient` and `ListCredits`/`Consume`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// OpenAIClient talks to the ChatGPT backend. It has NO CPA awareness.
// BaseURL defaults to "https://chatgpt.com" and is only overridable for tests.
type OpenAIClient struct {
	BaseURL string
	HTTP    *http.Client
}

const defaultBaseURL = "https://chatgpt.com"

func (c *OpenAIClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

func (c *OpenAIClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *OpenAIClient) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("openai %s %s: status %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

func (c *OpenAIClient) setAuthHeaders(req *http.Request, creds CodexCredentials) {
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.ChatGPTAccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", creds.ChatGPTAccountID)
	}
	req.Header.Set("User-Agent", codexUserAgent)
}

// ListCredits calls GET /backend-api/wham/rate-limit-reset-credits.
func (c *OpenAIClient) ListCredits(creds CodexCredentials) (Snapshot, error) {
	u := c.baseURL() + resetCreditsEndpointPath
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return Snapshot{}, err
	}
	c.setAuthHeaders(req, creds)
	req.Header.Set("Accept", "application/json")
	body, err := c.do(req)
	if err != nil {
		return Snapshot{}, err
	}
	return parseResetCreditsResponse(body, time.Now())
}

// GetUsage calls GET /backend-api/wham/usage.
func (c *OpenAIClient) GetUsage(creds CodexCredentials) (Snapshot, error) {
	u := c.baseURL() + usageEndpointPath
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return Snapshot{}, err
	}
	c.setAuthHeaders(req, creds)
	req.Header.Set("Accept", "application/json")
	body, err := c.do(req)
	if err != nil {
		return Snapshot{}, err
	}
	return parseUsageResponse(body, time.Now())
}

// Consume calls POST /backend-api/wham/rate-limit-reset-credits/consume with
// the targeted credit_id form (spec §2.1). Body is byte-exact: only
// redeem_request_id and credit_id are present.
func (c *OpenAIClient) Consume(creds CodexCredentials, redeemRequestID, creditID string) (ConsumeResponse, error) {
	u := c.baseURL() + consumeEndpointPath
	payload := struct {
		RedeemRequestID string `json:"redeem_request_id"`
		CreditID        string `json:"credit_id"`
	}{RedeemRequestID: redeemRequestID, CreditID: creditID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ConsumeResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return ConsumeResponse{}, err
	}
	c.setAuthHeaders(req, creds)
	req.Header.Set("Content-Type", "application/json")
	body, err := c.do(req)
	if err != nil {
		return ConsumeResponse{}, err
	}
	return parseConsumeResponse(body)
}

// Endpoint paths are constants relative to BaseURL. The production BaseURL is
// https://chatgpt.com, so the full URL matches spec §2.1 exactly.
const (
	usageEndpointPath        = "/backend-api/wham/usage"
	resetCreditsEndpointPath = "/backend-api/wham/rate-limit-reset-credits"
	consumeEndpointPath      = "/backend-api/wham/rate-limit-reset-credits/consume"
)

// urlJoin is a thin helper retained for future flexibility; not used today.
func urlJoin(base, path string) string {
	bu, err := url.Parse(base)
	if err != nil {
		return base + path
	}
	bu.Path = path
	return bu.String()
}
```

Note: the `usageEndpoint`/`resetCreditsEndpoint`/`consumeEndpoint` constants from Task 1 are the **full** URLs for documentation; `openai_client.go` uses the `*Path` suffix constants for actual requests so tests can override `BaseURL`. Keep both — the full-URL constants are referenced by tests in Task 1 to lock the documented contract.

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestListCredits|TestConsume' ./...`
Expected: PASS.

- [ ] **Step 5: Add a test that the full-URL constants equal BaseURL + path**

Append to `openai_client_test.go`:

```go
func TestFullURLEqualsDefaultBasePlusPath(t *testing.T) {
	if defaultBaseURL+resetCreditsEndpointPath != resetCreditsEndpoint {
		t.Fatalf("reset-credits URL mismatch")
	}
	if defaultBaseURL+consumeEndpointPath != consumeEndpoint {
		t.Fatalf("consume URL mismatch")
	}
	if defaultBaseURL+usageEndpointPath != usageEndpoint {
		t.Fatalf("usage URL mismatch")
	}
}
```

- [ ] **Step 6: Run all openai_client tests**

Run: `go test -run 'TestListCredits|TestConsume|TestFullURL' ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add openai_client.go openai_client_test.go
git commit -m "feat: add OpenAI HTTP client with byte-exact wire format"
```

---

## Task 6: Retry policy

**Files:**
- Create: `retry.go`
- Test: `retry_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
)

func TestBackoffForAttempt(t *testing.T) {
	want := []int{1, 2, 5, 10, 30}
	for i, minutes := range want {
		got := backoffForAttempt(i + 1)
		if int(got.Minutes()) != minutes {
			t.Fatalf("attempt %d => %v, want %dm", i+1, got, minutes)
		}
	}
	// Out of range clamps to last value.
	if got := backoffForAttempt(99); got != resetRetryDelays[len(resetRetryDelays)-1] {
		t.Fatalf("attempt 99 => %v", got)
	}
}

func TestClassifyConsumeResponse(t *testing.T) {
	cases := []struct {
		name string
		resp ConsumeResponse
		err  error
		want retryDecision
	}{
		{"reset", ConsumeResponse{Code: ConsumeCodeReset}, nil, decisionSuccess},
		{"already_redeemed", ConsumeResponse{Code: ConsumeCodeAlreadyRedeemed}, nil, decisionSuccess},
		{"no_credit", ConsumeResponse{Code: ConsumeCodeNoCredit}, nil, decisionStop},
		{"nothing_to_reset", ConsumeResponse{Code: ConsumeCodeNothingToReset}, nil, decisionStop},
		{"network_err", ConsumeResponse{}, errNetwork, decisionRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyConsume(tc.resp, tc.err)
			if got != tc.want {
				t.Fatalf("classify %s => %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// sentinel error used only by tests
var errNetwork = &testErr{"network"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestBackoff|TestClassify' ./...`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import "time"

// retryDecision is what the FSM should do after a consume attempt (spec §5.3).
type retryDecision int

const (
	decisionSuccess retryDecision = iota // code=reset or already_redeemed -> VERIFYING
	decisionRetry                        // transient error -> retry same UUID
	decisionStop                         // no_credit / nothing_to_reset / unknown -> DONE
)

func backoffForAttempt(attempt int) time.Duration {
	if attempt < 1 || attempt > len(resetRetryDelays) {
		if len(resetRetryDelays) == 0 {
			return 0
		}
		return resetRetryDelays[len(resetRetryDelays)-1]
	}
	return resetRetryDelays[attempt-1]
}

func classifyConsume(resp ConsumeResponse, err error) retryDecision {
	if err != nil {
		return decisionRetry // network / 5xx / timeout
	}
	switch resp.Code {
	case ConsumeCodeReset, ConsumeCodeAlreadyRedeemed:
		return decisionSuccess
	case ConsumeCodeNoCredit, ConsumeCodeNothingToReset:
		return decisionStop
	default:
		return decisionStop // unknown code: do not hammer the server
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestBackoff|TestClassify' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add retry.go retry_test.go
git commit -m "feat: add retry backoff and consume-response classification"
```

---

## Task 7: Config — 3 user fields

**Files:**
- Create: `config.go`
- Test: `config_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RefreshInterval != 12*time.Hour {
		t.Fatalf("R = %v", cfg.RefreshInterval)
	}
	if cfg.TriggerLeadTime != 6*time.Hour {
		t.Fatalf("L = %v", cfg.TriggerLeadTime)
	}
	if len(cfg.EnabledAccounts) != 0 {
		t.Fatalf("enabled = %v", cfg.EnabledAccounts)
	}
}

func TestParseConfigYAML(t *testing.T) {
	yaml := []byte(`
refresh_interval: 4h
trigger_lead_time: 2h
enabled_accounts: [a, b]
`)
	cfg, err := parseConfigYAML(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.RefreshInterval != 4*time.Hour || cfg.TriggerLeadTime != 2*time.Hour {
		t.Fatalf("parsed = %+v", cfg)
	}
	if len(cfg.EnabledAccounts) != 2 || cfg.EnabledAccounts[0] != "a" {
		t.Fatalf("enabled = %v", cfg.EnabledAccounts)
	}
}

func TestNormalizeConfig_Floors(t *testing.T) {
	cfg := Config{RefreshInterval: 1 * time.Minute, TriggerLeadTime: 0, EnabledAccounts: []string{"x"}}
	cfg = normalizeConfig(cfg)
	// Floor intervals to 1 minute to avoid tight loops.
	if cfg.RefreshInterval < time.Minute {
		t.Fatalf("R not floored: %v", cfg.RefreshInterval)
	}
	if cfg.TriggerLeadTime < time.Minute {
		t.Fatalf("L not floored: %v", cfg.TriggerLeadTime)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestDefaultConfig|TestParseConfigYAML|TestNormalizeConfig' ./...`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the 3 user-tunable fields (spec §7.1).
type Config struct {
	RefreshInterval time.Duration `yaml:"refresh_interval" json:"refresh_interval"`
	TriggerLeadTime time.Duration `yaml:"trigger_lead_time" json:"trigger_lead_time"`
	EnabledAccounts []string      `yaml:"enabled_accounts" json:"enabled_accounts"`
}

func DefaultConfig() Config {
	return Config{
		RefreshInterval: 12 * time.Hour,
		TriggerLeadTime: 6 * time.Hour,
		EnabledAccounts: nil,
	}
}

func parseConfigYAML(raw []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config yaml: %w", err)
	}
	return normalizeConfig(cfg), nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.RefreshInterval < time.Minute {
		cfg.RefreshInterval = time.Minute
	}
	if cfg.TriggerLeadTime < time.Minute {
		cfg.TriggerLeadTime = time.Minute
	}
	out := make([]string, 0, len(cfg.EnabledAccounts))
	seen := map[string]bool{}
	for _, a := range cfg.EnabledAccounts {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	cfg.EnabledAccounts = out
	return cfg
}
```

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestDefaultConfig|TestParseConfigYAML|TestNormalizeConfig' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add 3-field user config with YAML parsing"
```

---

## Task 8: FSM engine — per-account state machine

**Files:**
- Create: `resetbank.go`
- Test: `resetbank_test.go`

This is the production version of the FSM already validated by `sim/engine.go`. It must produce identical behavior. Keep the engine deterministic: inject `now func() time.Time`, an interface for the OpenAI client, and a logger.

- [ ] **Step 1: Define the FSM struct and OpenAI client interface**

```go
package main

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ResetClient is the subset of OpenAIClient the FSM needs. An interface keeps
// the FSM unit-testable with a fake.
type ResetClient interface {
	ListCredits(creds CodexCredentials) (Snapshot, error)
	GetUsage(creds CodexCredentials) (Snapshot, error)
	Consume(creds CodexCredentials, redeemRequestID, creditID string) (ConsumeResponse, error)
}

// AccountFSM is one account's state machine (spec §4).
type AccountFSM struct {
	AuthID   string
	Creds    CodexCredentials
	Cfg      Config
	Client   ResetClient
	Now      func() time.Time
	Logf     func(level, scope string, state State, msg, nextAction string, nextAt *time.Time, details map[string]any)

	state    State
	attempt  ResetAttempt
	nextWake time.Time // scheduled next step; zero = immediate
	verifyDue time.Time
}

func NewAccountFSM(authID string, creds CodexCredentials, cfg Config, client ResetClient, now func() time.Time, logf func(...)) *AccountFSM {
	return &AccountFSM{
		AuthID: authID, Creds: creds, Cfg: cfg, Client: client, Now: now,
		Logf: adaptLogf(logf),
		state: StateIDLE,
	}
}
```

(Note: `adaptLogf` and the exact `Logf` signature are finalized in Step 3. For now, define `Logf` as the 7-argument form shown; delete `adaptLogf`/the loose `logf func(...)` and pass `Logf` directly. Simpler: have `NewAccountFSM` accept the 7-arg `Logf` directly.)

Replace the constructor with:

```go
type LogFn func(level, scope string, state State, msg, nextAction string, nextAt *time.Time, details map[string]any)

func NewAccountFSM(authID string, creds CodexCredentials, cfg Config, client ResetClient, now func() time.Time, logf LogFn) *AccountFSM {
	return &AccountFSM{
		AuthID: authID, Creds: creds, Cfg: cfg, Client: client, Now: now, Logf: logf,
		state: StateIDLE,
	}
}
```

- [ ] **Step 2: Add `go.mod` dep on google/uuid**

Run: `go get github.com/google/uuid`
This provides `uuid.NewString()` for `redeem_request_id`. Add to `go.mod` requires.

- [ ] **Step 3: Write the failing test — happy path through all 6 states**

```go
package main

import (
	"errors"
	"testing"
	"time"
)

// fakeClient implements ResetClient for tests.
type fakeClient struct {
	credits   map[string]*Credit
	weeklyPct int
	consumeResp ConsumeResponse
	consumeErr  error
	listCalls   int
	consumeCalls int
	consumeIDs  []string
	redeemIDs   []string
}

func (f *fakeClient) ListCredits(CodexCredentials) (Snapshot, error) {
	f.listCalls++
	credits := make([]Credit, 0, len(f.credits))
	for _, c := range f.credits {
		credits = append(credits, *c)
	}
	return Snapshot{Credits: credits, AvailableCount: countAvail(credits), WeeklyPct: f.weeklyPct}, nil
}

func (f *fakeClient) GetUsage(CodexCredentials) (Snapshot, error) {
	return Snapshot{WeeklyPct: f.weeklyPct, AvailableCount: countAvail(snapshotCredits(f.credits))}, nil
}

func (f *fakeClient) Consume(_ CodexCredentials, rrid, cid string) (ConsumeResponse, error) {
	f.consumeCalls++
	f.consumeIDs = append(f.consumeIDs, cid)
	f.redeemIDs = append(f.redeemIDs, rrid)
	if f.consumeErr != nil {
		return ConsumeResponse{}, f.consumeErr
	}
	if f.consumeResp.Code == ConsumeCodeReset || f.consumeResp.Code == ConsumeCodeAlreadyRedeemed {
		if c, ok := f.credits[cid]; ok {
			c.Status = "redeemed"
		}
		f.weeklyPct = 100
	}
	return f.consumeResp, nil
}

func countAvail(cs []Credit) int { n := 0; for _, c := range cs { if c.Status == "available" { n++ } }; return n }
func snapshotCredits(m map[string]*Credit) []Credit { out := []Credit{}; for _, c := range m { out = append(out, *c) }; return out }

func TestFSM_HappyPath_AllStates(t *testing.T) {
	E := time.Date(2026, 7, 19, 18, 23, 47, 0, time.UTC)
	cfg := Config{RefreshInterval: 6 * time.Hour, TriggerLeadTime: 6 * time.Hour}
	fc := &fakeClient{
		credits: map[string]*Credit{"c1": {ID: "c1", Status: "available", ExpiresAt: E}},
		weeklyPct: 15, consumeResp: ConsumeResponse{Code: ConsumeCodeReset, WindowsReset: 2},
	}
	mu := &fakeClock{t: E.Add(-12 * time.Hour)}
	logs := []string{}
	logf := func(level, scope string, state State, msg, next string, nextAt *time.Time, d map[string]any) {
		logs = append(logs, fmt.Sprintf("%s/%s: %s", level, state, msg))
	}
	fsm := NewAccountFSM("acct1", CodexCredentials{AccessToken: "t"}, cfg, fc, mu.Now, logf)

	// Drive manually. Each Step returns the next scheduled wake.
	for i := 0; i < 50 && fsm.State() != StateDONE; i++ {
		next := fsm.Step()
		if next.IsZero() {
			break
		}
		mu.t = next
	}

	if fsm.State() != StateDONE {
		t.Fatalf("final state = %s", fsm.State())
	}
	if fc.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", fc.consumeCalls)
	}
	if fc.consumeIDs[0] != "c1" {
		t.Fatalf("consumed = %v", fc.consumeIDs)
	}
	if c := fc.credits["c1"]; c.Status != "redeemed" {
		t.Fatalf("c1 status = %s", c.Status)
	}
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

var _ = errors.New
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test -run TestFSM_HappyPath ./...`
Expected: FAIL — undefined `Step`, `State()`, `fmt` import.

- [ ] **Step 5: Implement `State()` accessor and `Step()`**

Append to `resetbank.go`:

```go
import "fmt" // merge into existing imports

func (f *AccountFSM) State() State { return f.state }

func (f *AccountFSM) NextWake() time.Time { return f.nextWake }

// Step executes one FSM tick at f.Now() and returns the next scheduled wake.
// Zero return means the cycle terminated (DONE).
func (f *AccountFSM) Step() time.Time {
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

func (f *AccountFSM) triggerTime(c Credit) time.Time { return c.ExpiresAt.Add(-f.Cfg.TriggerLeadTime) }
func (f *AccountFSM) armThreshold(c Credit) time.Time { return f.triggerTime(c).Add(-f.Cfg.RefreshInterval) }

func (f *AccountFSM) log(state State, level, msg, nextAction string, nextAt *time.Time, details map[string]any) {
	if f.Logf != nil {
		f.Logf(level, f.AuthID, state, msg, nextAction, nextAt, details)
	}
}

func (f *AccountFSM) stepIDLE() time.Time {
	now := f.now()
	snap, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		next := now.Add(f.Cfg.RefreshInterval)
		f.log(StateIDLE, "warn", fmt.Sprintf("list failed: %v", err), "retry patrol", &next, nil)
		f.nextWake = next
		return next
	}
	avail := availableCreditsSorted(snap.Credits, now)
	if len(avail) == 0 {
		next := now.Add(f.Cfg.RefreshInterval)
		f.log(StateIDLE, "info", "no available credits", "patrol", &next, nil)
		f.nextWake = next
		return next
	}
	target := avail[0]
	M := f.armThreshold(target)
	if now.Before(M) {
		next := now.Add(f.Cfg.RefreshInterval)
		f.log(StateIDLE, "info",
			fmt.Sprintf("credit %s not near trigger window (M=%s)", target.ID, M.Format(time.RFC3339)),
			"patrol", &next, map[string]any{"credit_id": target.ID, "expires_at": target.ExpiresAt})
		f.nextWake = next
		return next
	}
	// Enter ARMED. Wake at T, or now if already past T.
	T := f.triggerTime(target)
	f.state = StateARMED
	wake := T
	if !now.Before(T) {
		wake = now
	}
	f.log(StateARMED, "warn",
		fmt.Sprintf("credit %s armed; trigger window T=%s", target.ID, T.Format(time.RFC3339)),
		"sleep to T then confirm", &wake, map[string]any{"credit_id": target.ID, "T": T})
	f.nextWake = wake
	return wake
}

func (f *AccountFSM) stepARMED() time.Time {
	// ARMED only sleeps to T; the wake has already happened. Move to CONFIRMING.
	f.state = StateCONFIRMING
	return f.now() // re-enter immediately
}

func (f *AccountFSM) stepCONFIRMING() time.Time {
	now := f.now()
	snap, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log(StateCONFIRMING, "error", fmt.Sprintf("confirm list failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	avail := availableCreditsSorted(snap.Credits, now)
	if len(avail) == 0 {
		f.state = StateDONE
		f.log(StateCONFIRMING, "warn", "target credit vanished before reset", "abandon", nil, nil)
		return time.Time{}
	}
	target := avail[0]
	f.attempt = ResetAttempt{
		RedeemRequestID: uuid.NewString(),
		TargetCreditID:  target.ID,
		PreSnapshot:     snap,
		StartedAt:       now,
		Attempt:         0,
	}
	f.state = StateRESETTING
	f.log(StateCONFIRMING, "info",
		fmt.Sprintf("confirmed target=%s, pre weekly=%d%%, redeem_id=%s", target.ID, snap.WeeklyPct, f.attempt.RedeemRequestID),
		"send POST /consume", nil, map[string]any{"credit_id": target.ID})
	return f.now()
}

func (f *AccountFSM) stepRESETTING() time.Time {
	now := f.now()
	resp, err := f.Client.Consume(f.Creds, f.attempt.RedeemRequestID, f.attempt.TargetCreditID)
	switch classifyConsume(resp, err) {
	case decisionSuccess:
		f.state = StateVERIFYING
		f.verifyDue = now.Add(postResetVerifyDelay)
		f.log(StateRESETTING, "info",
			fmt.Sprintf("consume ok code=%s windows_reset=%d", resp.Code, resp.WindowsReset),
			fmt.Sprintf("verify in %v", postResetVerifyDelay), &f.verifyDue,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "credit_id": f.attempt.TargetCreditID})
		f.nextWake = f.verifyDue
		return f.verifyDue
	case decisionStop:
		f.state = StateDONE
		msg := fmt.Sprintf("consume returned %s", resp.Code)
		if err != nil {
			msg = fmt.Sprintf("consume error: %v", err)
		}
		f.log(StateRESETTING, "warn", msg, "abandon", nil, map[string]any{"redeem_request_id": f.attempt.RedeemRequestID})
		return time.Time{}
	case decisionRetry:
		f.attempt.Attempt++
		if f.attempt.Attempt > len(resetRetryDelays) {
			f.state = StateDONE
			f.log(StateRESETTING, "error",
				fmt.Sprintf("retries exhausted after %d attempts; last error: %v", f.attempt.Attempt-1, err),
				"abandon", nil, map[string]any{"redeem_request_id": f.attempt.RedeemRequestID})
			return time.Time{}
		}
		backoff := backoffForAttempt(f.attempt.Attempt)
		f.attempt.NextRetryAt = now.Add(backoff)
		f.log(StateRESETTING, "warn",
			fmt.Sprintf("consume failed (attempt %d): %v; will retry with same UUID", f.attempt.Attempt, err),
			fmt.Sprintf("retry in %v (same redeem_request_id)", backoff), &f.attempt.NextRetryAt,
			map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "attempt": f.attempt.Attempt})
		f.nextWake = f.attempt.NextRetryAt
		return f.attempt.NextRetryAt
	}
	return time.Time{}
}

func (f *AccountFSM) stepVERIFYING() time.Time {
	now := f.now()
	credits, err := f.Client.ListCredits(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log(StateVERIFYING, "error", fmt.Sprintf("verify list failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	usage, err := f.Client.GetUsage(f.Creds)
	if err != nil {
		f.state = StateDONE
		f.log(StateVERIFYING, "error", fmt.Sprintf("verify usage failed: %v", err), "abandon", nil, nil)
		return time.Time{}
	}
	// Triple deduction check (spec §4.3 invariant 3).
	targetGone := true
	for _, c := range credits.Credits {
		if c.ID == f.attempt.TargetCreditID && c.Status == "available" {
			targetGone = false
		}
	}
	countDown1 := credits.AvailableCount == f.attempt.PreSnapshot.AvailableCount-1
	quotaUp := usage.WeeklyPct >= f.attempt.PreSnapshot.WeeklyPct && usage.WeeklyPct > 0
	f.state = StateDONE
	switch {
	case targetGone && countDown1 && quotaUp:
		f.log(StateVERIFYING, "info",
			fmt.Sprintf("verify OK: count %d->%d, weekly %d%%->%d%%", f.attempt.PreSnapshot.AvailableCount, credits.AvailableCount, f.attempt.PreSnapshot.WeeklyPct, usage.WeeklyPct),
			"cycle complete", nil, map[string]any{"credit_id": f.attempt.TargetCreditID})
	case targetGone && countDown1:
		f.log(StateVERIFYING, "warn",
			fmt.Sprintf("verify PARTIAL: count ok but weekly %d%%->%d%% (delayed reset?)", f.attempt.PreSnapshot.WeeklyPct, usage.WeeklyPct),
			"cycle complete", nil, nil)
	default:
		f.log(StateVERIFYING, "error",
			fmt.Sprintf("verify MISMATCH: targetGone=%v countDown1=%v quotaUp=%v — halting, no compensating request", targetGone, countDown1, quotaUp),
			"abandon", nil, map[string]any{"redeem_request_id": f.attempt.RedeemRequestID, "target_credit_id": f.attempt.TargetCreditID})
	}
	return time.Time{}
}
```

- [ ] **Step 6: Run the happy-path test**

Run: `go test -run TestFSM_HappyPath ./...`
Expected: PASS. If it fails, diff the FSM against `sim/engine.go` — the simulation already proved the logic; a mismatch is a transcription error.

- [ ] **Step 7: Add edge-case tests mirroring `sim/edge_test.go`**

Append tests for: idempotent retry (`FailFirstN` style on `fakeClient`), retries-exhausted, no_credit stop, credit-vanishes-during-ARMED, wrong-credit-consumed detection. Copy the assertions from `sim/edge_test.go` and adapt to the `AccountFSM.Step()` API. Each test should be <40 lines.

For each, follow the TDD micro-loop: write test → run (FAIL) → fix only if the production code is genuinely wrong (most should pass immediately since the logic matches the sim).

- [ ] **Step 8: Commit**

```bash
git add resetbank.go resetbank_test.go go.mod go.sum
git commit -m "feat: add per-account FSM engine (6 states, idempotent retry, triple-check)"
```

---

## Task 9: Log ring + persistence

**Files:**
- Create: `log.go`
- Test: `log_test.go`
- Create: `state.go`
- Test: `state_test.go`

- [ ] **Step 1: Write the failing test for the log ring**

```go
package main

import (
	"testing"
	"time"
)

func TestLogRing_CapsAndPrunes(t *testing.T) {
	r := newLogRing(maxLogEntries)
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxLogEntries+50; i++ {
		r.append(LogEntry{Timestamp: base.Add(time.Duration(i) * time.Minute), Level: "info", Scope: "x"})
	}
	all := r.all()
	if len(all) != maxLogEntries {
		t.Fatalf("len = %d, want %d", len(all), maxLogEntries)
	}
	// Oldest 50 dropped; first kept entry is i=50.
	if !all[0].Timestamp.Equal(base.Add(50 * time.Minute)) {
		t.Fatalf("first entry = %v", all[0].Timestamp)
	}
}

func TestLogRing_PruneByAge(t *testing.T) {
	r := newLogRing(maxLogEntries)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.append(LogEntry{Timestamp: now.Add(-2 * logRetention), Level: "info"}) // too old
	r.append(LogEntry{Timestamp: now.Add(-1 * time.Hour), Level: "info"})    // keep
	r.pruneOlderThan(now.Add(-logRetention))
	all := r.all()
	if len(all) != 1 {
		t.Fatalf("after prune len = %d", len(all))
	}
}
```

- [ ] **Step 2: Run, verify FAIL, implement `log.go`**

```go
package main

import (
	"sync"
	"time"
)

type logRing struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
}

func newLogRing(capacity int) *logRing {
	if capacity < 1 {
		capacity = 1
	}
	return &logRing{cap: capacity}
}

func (r *logRing) append(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	if len(r.entries) > r.cap {
		r.entries = r.entries[len(r.entries)-r.cap:]
	}
}

func (r *logRing) all() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *logRing) pruneOlderThan(cutoff time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if !e.Timestamp.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	r.entries = kept
}
```

- [ ] **Step 3: Run test, commit log ring**

Run: `go test -run TestLogRing ./...` → PASS.
```bash
git add log.go log_test.go
git commit -m "feat: add capped, age-pruned log ring"
```

- [ ] **Step 4: Write failing test for state persistence**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestState_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := &PluginState{
		Config: Config{RefreshInterval: 8 * time.Hour, TriggerLeadTime: 4 * time.Hour, EnabledAccounts: []string{"a"}},
		Accounts: map[string]AccountRuntime{
			"a": {AuthID: "a", State: StateARMED, NextWake: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)},
		},
	}
	if err := saveState(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	loaded, err := loadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Config.RefreshInterval != 8*time.Hour {
		t.Fatalf("R = %v", loaded.Config.RefreshInterval)
	}
	if loaded.Accounts["a"].State != StateARMED {
		t.Fatalf("acct state = %s", loaded.Accounts["a"].State)
	}
}
```

- [ ] **Step 5: Run, verify FAIL, implement `state.go`**

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// PluginState is the on-disk persistent shape.
type PluginState struct {
	Config   Config                     `json:"config"`
	Accounts map[string]AccountRuntime  `json:"accounts"`
}

// AccountRuntime is the persisted per-account FSM snapshot.
type AccountRuntime struct {
	AuthID   string    `json:"auth_id"`
	State    State     `json:"state"`
	NextWake time.Time `json:"next_wake"`
	Attempt  ResetAttempt `json:"attempt,omitempty"`
}

func saveState(path string, s *PluginState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadState(path string) (*PluginState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &PluginState{Accounts: map[string]AccountRuntime{}}, nil
		}
		return nil, err
	}
	var s PluginState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Accounts == nil {
		s.Accounts = map[string]AccountRuntime{}
	}
	return &s, nil
}
```

- [ ] **Step 6: Run, commit**

Run: `go test -run TestState_SaveLoadRoundTrip ./...` → PASS.
```bash
git add state.go state_test.go
git commit -m "feat: add JSON state persistence with atomic writes"
```

---

## Task 10: Worker — single goroutine time wheel

**Files:**
- Create: `worker.go`
- Test: `worker_test.go`

- [ ] **Step 1: Write failing test for the worker driving two accounts**

```go
package main

import (
	"sync"
	"testing"
	"time"
)

func TestWorker_DrivesMultipleAccountsToDone(t *testing.T) {
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
	w.testAccelerate = true // don't sleep in real time; jump clock

	var mu sync.Mutex
	_ = mu
	done := make(chan struct{})
	go func() { w.Run(); close(done) }()

	// Wait until both accounts reach DONE.
	deadline := time.After(5 * time.Second)
	for {
		got := 0
		w.EachFSM(func(fsm *AccountFSM) { if fsm.State() == StateDONE { got++ } })
		if got == 2 {
			w.Stop()
			<-done
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout: states not DONE; got=%d", got)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

var errUnknownAccount = errString("unknown account")
type errString string
func (e errString) Error() string { return string(e) }
```

- [ ] **Step 2: Run, verify FAIL**

Run: `go test -run TestWorker_DrivesMultipleAccountsToDone ./...`
Expected: FAIL — undefined `NewWorker`, `Worker.Run`, `Stop`, `EachFSM`, `testAccelerate`.

- [ ] **Step 3: Implement `worker.go`**

```go
package main

import (
	"sort"
	"sync"
	"time"
)

// CredsLoader returns the current credentials and client for an account.
// The worker calls this before every step to pick up CPA-side token refreshes.
type CredsLoader func(authID string) (CodexCredentials, ResetClient, error)

type Worker struct {
	cfg      Config
	enabled  []string
	loader   CredsLoader
	nowFn    func() time.Time

	mu       sync.Mutex
	fsms     map[string]*AccountFSM
	logs     *logRing
	stopCh   chan struct{}
	stopped  bool

	// testAccelerate makes Run skip real sleeping; tests drive the clock via nowFn.
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

func (w *Worker) EachFSM(fn func(*AccountFSM)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := make([]string, 0, len(w.fsms))
	for k := range w.fsms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fn(w.fsms[k])
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

// TriggerCheck forces nextWake=now for one (or all) accounts.
func (w *Worker) TriggerCheck(authID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if authID == "" {
		for _, fsm := range w.fsms {
			fsm.nextWake = w.nowFn()
		}
		return
	}
	if fsm, ok := w.fsms[authID]; ok {
		fsm.nextWake = w.nowFn()
	}
}

// Run blocks until Stop. It picks the soonest nextWake across accounts,
// sleeps until then, and steps that account.
func (w *Worker) Run() {
	// Initialize FSMs.
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
			// Nothing to do; wait briefly or until stopped.
			w.sleepOrStop(time.Second)
			continue
		}
		now := w.nowFn()
		if nextWake.After(now) && !w.testAccelerate {
			w.sleepOrStop(nextWake.Sub(now))
			continue
		}
		// Step the account. Refresh creds first.
		w.mu.Lock()
		fsm := w.fsms[nextAcct]
		w.mu.Unlock()
		if fsm == nil {
			continue
		}
		creds, client, err := w.loader(nextAcct)
		if err != nil {
			w.logs.append(LogEntry{Timestamp: now, Level: "error", Scope: nextAcct, State: fsm.State(), Message: "credential load failed: " + err.Error()})
			fsm.nextWake = now.Add(w.cfg.RefreshInterval)
			continue
		}
		fsm.Creds = creds
		fsm.Client = client
		wake := fsm.Step()
		if wake.IsZero() && fsm.State() == StateDONE {
			// Cycle complete; reset to IDLE for the next patrol.
			fsm.Reset()
			fsm.nextWake = w.nowFn().Add(w.cfg.RefreshInterval)
		} else {
			fsm.nextWake = wake
		}
	}
}

func (w *Worker) ensureFSM(id string) {
	if _, ok := w.fsms[id]; ok {
		return
	}
	creds, client, err := w.loader(id)
	if err != nil {
		w.logs.append(LogEntry{Timestamp: w.nowFn(), Level: "warn", Scope: id, Message: "initial credential load failed: " + err.Error()})
		return
	}
	w.fsms[id] = NewAccountFSM(id, creds, w.cfg, client, w.nowFn, w.makeLogFn(id))
}

func (w *Worker) makeLogFn(authID string) LogFn {
	return func(level, scope string, state State, msg, next string, nextAt *time.Time, details map[string]any) {
		w.logs.append(LogEntry{
			Timestamp: w.nowFn(), Level: level, Scope: scope, State: state,
			Message: msg, NextAction: next, NextAt: nextAt, Details: details,
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

// Reset returns a DONE FSM to IDLE for the next patrol cycle.
func (f *AccountFSM) Reset() {
	f.state = StateIDLE
	f.attempt = ResetAttempt{}
	f.verifyDue = time.Time{}
}
```

- [ ] **Step 4: Run worker test**

Run: `go test -run TestWorker_DrivesMultipleAccountsToDone ./...`
Expected: PASS.

- [ ] **Step 5: Add a test for TriggerCheck (manual-check deferral)**

Append: a test that starts a worker, calls `TriggerCheck("a")`, and asserts account "a"'s `nextWake` is reset to `now + RefreshInterval` after its check completes (spec §6.3 deferral rule). Drive with `testAccelerate=true` and a fake clock.

- [ ] **Step 6: Commit**

```bash
git add worker.go worker_test.go
git commit -m "feat: add single-goroutine worker with time wheel and manual triggers"
```

---

## Task 11: Management API routes

**Files:**
- Create: `management.go`
- Test: `management_test.go`

Implements spec §9 routes. The resource HTML is added in Task 12; here we wire JSON routes only.

- [ ] **Step 1: Write failing test for status and settings routes**

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestManagement_StatusRoute(t *testing.T) {
	srv := newTestManagement(t, Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour, EnabledAccounts: []string{"a"}}, []string{"a"})
	resp := srv.get(t, "/v0/management/plugins/codex-auto-reset/status?format=json")
	var body map[string]any
	if err := json.Unmarshal(resp, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["plugin"] != "codex-auto-reset" {
		t.Fatalf("body = %v", body)
	}
}

func TestManagement_PutSettingsUpdatesConfig(t *testing.T) {
	srv := newTestManagement(t, DefaultConfig(), nil)
	newCfg := []byte(`{"refresh_interval":"4h","trigger_lead_time":"2h","enabled_accounts":["x"]}`)
	resp := srv.put(t, "/v0/management/plugins/codex-auto-reset/settings", newCfg)
	var body map[string]any
	json.Unmarshal(resp, &body)
	if body["refresh_interval"] != "4h" {
		t.Fatalf("settings echo = %v", body)
	}
}

// helper type and constructors defined in management_test.go shared file.
type testMgmt struct {
	hand *managementHandlers
}

func newTestManagement(t *testing.T, cfg Config, enabled []string) *testMgmtHelper {
	// defined in step 3
	return nil
}

func (m *testMgmt) get(t *testing.T, path string) []byte   { return nil }
func (m *testMgmt) put(t *testing.T, path string, b []byte) []byte { return nil }

var _ = context.Background
var _ = http.MethodGet
```

(These stub helpers are placeholders for the test author; in Step 3 we write a real `testMgmtHelper` that drives `handleManagement` directly without HTTP.)

- [ ] **Step 2: Run, verify FAIL**

Run: `go test -run 'TestManagement_' ./...`
Expected: FAIL — undefined `newTestManagement`, `managementHandlers`, `testMgmtHelper`.

- [ ] **Step 3: Implement `management.go` (routes + JSON handlers; HTML deferred)**

```go
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const pluginID = "codex-auto-reset"

type managementHandlers struct {
	mu       sync.Mutex
	state    *PluginState
	statePath string
	worker   *Worker
}

func newManagementHandlers(state *PluginState, statePath string, worker *Worker) *managementHandlers {
	return &managementHandlers{state: state, statePath: statePath, worker: worker}
}

func (h *managementHandlers) handle(method, path string, headers http.Header, body []byte) (status int, respBody []byte) {
	switch {
	case method == http.MethodGet && path == "/codex-auto-reset/status":
		return h.statusJSON()
	case method == http.MethodGet && path == "/codex-auto-reset/logs":
		return h.logsJSON()
	case method == http.MethodPut && path == "/codex-auto-reset/settings":
		return h.putSettings(body)
	case method == http.MethodPost && path == "/codex-auto-reset/check":
		return h.checkOne(body)
	case method == http.MethodPost && path == "/codex-auto-reset/check/all":
		if h.worker != nil {
			h.worker.TriggerCheck("")
		}
		return jsonOK(map[string]any{"triggered": "all"})
	case method == http.MethodPost && path == "/codex-auto-reset/reset":
		return h.resetOne(body)
	case method == http.MethodGet && path == "/codex-auto-reset/export":
		return h.exportState()
	case method == http.MethodPost && path == "/codex-auto-reset/import":
		return h.importState(body)
	case method == http.MethodGet && path == "/codex-auto-reset/resource/status":
		return h.statusHTML()
	}
	return jsonStatus(http.StatusNotFound, map[string]any{"error": "route not found"})
}

func (h *managementHandlers) statusJSON() (int, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]any{
		"plugin": pluginID,
		"config": h.state.Config,
		"accounts": h.state.Accounts,
	}
	return jsonOK(out)
}

func (h *managementHandlers) putSettings(body []byte) (int, []byte) {
	var incoming Config
	if err := json.Unmarshal(body, &incoming); err != nil {
		// Try YAML-as-string fallback for duration fields.
		incoming2, err2 := parseConfigYAML(body)
		if err2 != nil {
			return jsonStatus(http.StatusBadRequest, map[string]any{"error": "invalid config: " + err.Error()})
		}
		incoming = incoming2
	}
	incoming = normalizeConfig(incoming)
	h.mu.Lock()
	h.state.Config = incoming
	h.mu.Unlock()
	if err := saveState(h.statePath, h.state); err != nil {
		return jsonStatus(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return jsonOK(map[string]any{"refresh_interval": incoming.RefreshInterval.String(), "trigger_lead_time": incoming.TriggerLeadTime.String(), "enabled_accounts": incoming.EnabledAccounts})
}

func (h *managementHandlers) logsJSON() (int, []byte) {
	var entries []LogEntry
	if h.worker != nil {
		entries = h.worker.Logs().all()
	}
	return jsonOK(map[string]any{"entries": entries})
}

func (h *managementHandlers) checkOne(body []byte) (int, []byte) {
	var req struct{ AuthIndex string `json:"auth_index"` }
	_ = json.Unmarshal(body, &req)
	if h.worker != nil {
		h.worker.TriggerCheck(req.AuthIndex)
	}
	return jsonOK(map[string]any{"triggered": req.AuthIndex})
}

func (h *managementHandlers) resetOne(body []byte) (int, []byte) {
	var req struct{ AuthIndex string `json:"auth_index"` }
	if err := json.Unmarshal(body, &req); err != nil || req.AuthIndex == "" {
		return jsonStatus(http.StatusBadRequest, map[string]any{"error": "auth_index required"})
	}
	if h.worker != nil {
		h.worker.ForceConfirm(req.AuthIndex)
	}
	return jsonOK(map[string]any{"forced": req.AuthIndex})
}

func (h *managementHandlers) exportState() (int, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, _ := json.MarshalIndent(h.state, "", "  ")
	return http.StatusOK, raw
}

func (h *managementHandlers) importState(body []byte) (int, []byte) {
	var s PluginState
	if err := json.Unmarshal(body, &s); err != nil {
		return jsonStatus(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	s.Config = normalizeConfig(s.Config)
	h.mu.Lock()
	h.state.Config = s.Config
	h.state.Accounts = s.Accounts
	h.mu.Unlock()
	if err := saveState(h.statePath, h.state); err != nil {
		return jsonStatus(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return jsonOK(map[string]any{"imported": true})
}

func (h *managementHandlers) statusHTML() (int, []byte) {
	return http.StatusOK, []byte(renderStatusHTML(h.state))
}

func jsonOK(v any) (int, []byte) {
	raw, err := json.Marshal(v)
	if err != nil {
		return jsonStatus(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return http.StatusOK, raw
}

func jsonStatus(status int, v any) (int, []byte) {
	raw, _ := json.Marshal(v)
	return status, raw
}

// unused but kept for future routing parity with scheduler.
var _ = strings.TrimSpace
var _ = time.Now
```

Add `ForceConfirm` to `worker.go`:

```go
// ForceConfirm advances an account's FSM to CONFIRMING (used by the manual reset button).
func (w *Worker) ForceConfirm(authID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if fsm, ok := w.fsms[authID]; ok {
		fsm.state = StateCONFIRMING
		fsm.nextWake = w.nowFn()
	}
}
```

- [ ] **Step 4: Rewrite the test helpers to drive `handle` directly**

Replace the stub `testMgmt`/`newTestManagement` with:

```go
type testMgmtHelper struct{ h *managementHandlers }

func newTestManagement(t *testing.T, cfg Config, enabled []string) *testMgmtHelper {
	t.Helper()
	state := &PluginState{Config: cfg, Accounts: map[string]AccountRuntime{}}
	for _, id := range enabled {
		state.Accounts[id] = AccountRuntime{AuthID: id, State: StateIDLE}
	}
	return &testMgmtHelper{h: newManagementHandlers(state, filepath.Join(t.TempDir(), "state.json"), nil)}
}

func (m *testMgmtHelper) get(t *testing.T, path string) []byte {
	t.Helper()
	status, body := m.h.handle(http.MethodGet, strings.TrimPrefix(path, "/v0/management/plugins/codex-auto-reset"), nil, nil)
	if status >= 400 {
		t.Fatalf("GET %s: status %d body %s", path, status, body)
	}
	return body
}

func (m *testMgmtHelper) put(t *testing.T, path string, b []byte) []byte {
	t.Helper()
	status, body := m.h.handle(http.MethodPut, strings.TrimPrefix(path, "/v0/management/plugins/codex-auto-reset"), nil, b)
	if status >= 400 {
		t.Fatalf("PUT %s: status %d body %s", path, status, body)
	}
	return body
}
```

(Add `path/filepath` and `strings` imports to the test file.)

- [ ] **Step 5: Run management tests**

Run: `go test -run 'TestManagement_' ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add management.go management_test.go worker.go
git commit -m "feat: add management API JSON routes (status/logs/settings/check/reset/import/export)"
```

---

## Task 12: Bilingual resource HTML

**Files:**
- Modify: `management.go` (add `renderStatusHTML`)
- Test: `management_test.go`

This mirrors the invite plugin's single-file HTML pattern (inline CSS+JS, i18n object). Keep it self-contained.

- [ ] **Step 1: Write failing test asserting HTML contains key markers**

```go
func TestStatusHTMLContainsBilingualMarkers(t *testing.T) {
	h := newManagementHandlers(&PluginState{Config: DefaultConfig(), Accounts: map[string]AccountRuntime{}}, "", nil)
	_, body := h.handle(http.MethodGet, "/codex-auto-reset/resource/status", nil, nil)
	html := string(body)
	for _, want := range []string{"codex-auto-reset", "data-i18n", "EN", "ZH", "nextCheckAt"} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML missing %q", want)
		}
	}
}
```

- [ ] **Step 2: Run, verify FAIL, implement `renderStatusHTML`**

Add a `renderStatusHTML(state *PluginState) string` function that returns a `<!doctype html>` page with:
- A header with title "Codex Auto Reset" and a language `<select>` (EN/ZH).
- A `data-i18n` translation object with EN and ZH keys for: title, config labels (refresh interval, lead time, enabled accounts), account card labels (state, credits, weekly %, last cycle, next cycle), and buttons (manual check, manual reset, view logs).
- A `<div id="app">` populated by JS that fetches `/v0/management/plugins/codex-auto-reset/status?format=json` and `/logs`.
- A management-key input (password) used as `Authorization: Bearer <key>` for all management fetches.
- Buttons that POST to `/check`, `/check/all`, `/reset`.
- Per-account card rendering showing state badge, credits list (target highlighted), weekly %, last log line, and next-cycle plan with countdown.
- Countdown: a `<span data-next="<RFC3339>">` whose text is updated every second by a `setInterval`.

Keep the HTML under ~300 lines; inline CSS in a `<style>` block, inline JS in a `<script>` block. Use vanilla JS (no build step). Follow the invite plugin's visual conventions (panels, metric chips, button styles).

- [ ] **Step 3: Run test**

Run: `go test -run TestStatusHTMLContainsBilingualMarkers ./...`
Expected: PASS.

- [ ] **Step 4: Manually sanity-check rendering**

Add a temporary `go run`-able main (in a `_manual/` subfolder with its own `//go:build manual` tag) that prints `renderStatusHTML` to stdout and visually inspect, then delete it. (Optional; the test is the gate.)

- [ ] **Step 5: Commit**

```bash
git add management.go management_test.go
git commit -m "feat: add bilingual resource HTML page with live status and manual triggers"
```

---

## Task 13: main.go — C ABI entry, host-callback wiring

**Files:**
- Create: `main.go`
- Test: `main_test.go` (light; most logic is behind interfaces already tested)

Copy the ABI boilerplate from `../cpa-plugin-codex-quota-scheduler/main.go` and adapt the `handleMethod` dispatch to our routes.

- [ ] **Step 1: Copy and adapt the ABI shell**

Copy `../cpa-plugin-codex-quota-scheduler/main.go`. Keep the C preamble (`cliproxy_buffer`, `cliproxy_host_api`, `callHostCallbackABI`, `writeResponse`, `errorEnvelope`) **unchanged**. Change:
- The plugin registration metadata: name "Codex Auto Reset", GitHub repository "https://github.com/jeffery/cpa-plugin-codex-auto-reset".
- The `globalWorker *Worker` global and its init in `cliproxy_plugin_init`.
- The method dispatch in `handleMethod`.

- [ ] **Step 2: Wire method dispatch**

```go
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistrationResponse{
			Routes: []pluginapi.ManagementRoute{
				{Method: http.MethodGet, Path: "/codex-auto-reset/status"},
				{Method: http.MethodGet, Path: "/codex-auto-reset/logs"},
				{Method: http.MethodPut, Path: "/codex-auto-reset/settings"},
				{Method: http.MethodPost, Path: "/codex-auto-reset/check"},
				{Method: http.MethodPost, Path: "/codex-auto-reset/check/all"},
				{Method: http.MethodPost, Path: "/codex-auto-reset/reset"},
				{Method: http.MethodGet, Path: "/codex-auto-reset/export"},
				{Method: http.MethodPost, Path: "/codex-auto-reset/import"},
			},
			Resources: []pluginapi.ResourceRoute{{
				Path: "/codex-auto-reset/resource/status", Menu: "Codex Auto Reset",
				Description: "Auto-redeem Codex Reset Bank credits before they expire.",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return okEnvelope(handleManagementRequest(request))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
```

(Define `configurePlugin`, `pluginRegistration`, `handleManagementRequest` in `main.go`. `configurePlugin` parses `lifecycleRequest{ConfigYAML}` via `parseConfigYAML`, constructs/updates a `Worker`, and starts it if not running. `handleManagementRequest` unmarshals `managementRequest`, calls `managementHandlers.handle`, and wraps the result in `pluginapi.ManagementResponse`.)

- [ ] **Step 3: Wire the credential loader via host ABI `host.auth.list`**

```go
func hostCredsLoader(authID string) (CodexCredentials, ResetClient, error) {
	auths, err := listAuthsViaHost()
	if err != nil {
		return CodexCredentials{}, nil, err
	}
	for _, a := range auths {
		if a.ID == authID || a.AuthIndex == authID {
			raw, err := getAuthViaHost(a.ID)
			if err != nil {
				return CodexCredentials{}, nil, err
			}
			creds, err := ExtractCodexCredentials(raw)
			if err != nil {
				return CodexCredentials{}, nil, err
			}
			return creds, &OpenAIClient{}, nil
		}
	}
	return CodexCredentials{}, nil, fmt.Errorf("auth %q not found", authID)
}
```

(`listAuthsViaHost` and `getAuthViaHost` call `callHostCallbackABI(pluginabi.MethodHostAuthList, ...)` and `MethodHostAuthGet`. Mirror the scheduler's `ABIHostClient.ListAuths` pattern.)

- [ ] **Step 4: Add a light main_test.go that verifies `pluginRegistration` metadata**

```go
package main

import "testing"

func TestPluginRegistration(t *testing.T) {
	reg := pluginRegistration()
	if reg.Metadata.Name != "Codex Auto Reset" {
		t.Fatalf("name = %q", reg.Metadata.Name)
	}
	if reg.Capabilities.ManagementAPI != true {
		t.Fatalf("management API not enabled")
	}
}
```

- [ ] **Step 5: Build the shared library**

Run: `make build`
Expected: `dist/codex-auto-reset.<ext>` is produced. On Windows, `.\build.ps1` is the alternative.

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add C ABI entry, host-callback credential loader, plugin lifecycle"
```

---

## Task 14: Release tooling — GitHub Actions workflow

**Files:**
- Create: `.github/workflows/build.yml`
- Create: `.github/scripts/package-release.go`

- [ ] **Step 1: Copy `package-release.go` from the scheduler**

Copy `../cpa-plugin-codex-quota-scheduler/.github/scripts/package-release.go` verbatim.

- [ ] **Step 2: Copy and adapt `build.yml`**

Copy `../cpa-plugin-codex-quota-scheduler/.github/workflows/build.yml`. Replace `PLUGIN_NAME: codex-quota-scheduler` with `PLUGIN_NAME: codex-auto-reset`. Keep the matrix (linux/darwin amd64+arm64, windows amd64), MinGW setup, and the tag-driven release (`v*` → VERSION = tag without `v` prefix).

- [ ] **Step 3: Lint the workflow**

Run: `make test && make vet`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add .github/
git commit -m "ci: add multi-platform build and release workflow"
```

---

## Task 15: README

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write the README**

Structure (mirror the scheduler README's sections, adapted):
- Title + one-paragraph summary ("auto-redeems soonest-expiring Codex Reset Bank credits").
- **Privacy And Data Disclosure** — same posture as scheduler: state-changing actions need CPA Management key; key stays only in browser session; the plugin sends authenticated requests only to `chatgpt.com/backend-api/wham/{usage,rate-limit-reset-credits,rate-limit-reset-credits/consume}`; no data sent to the plugin author.
- **Installation** — download zip, place `.so`/`.dll`/`.dylib` in CPA plugin dir.
- **CPA Configuration** — the `plugins.configs.codex-auto-reset` YAML block.
- **Configuration** — the 3 user fields with defaults and examples.
- **How It Works** — 1-paragraph plain-language summary of the state machine (refer to the spec for details).
- **Management UI** — resource page URL, what it shows, manual buttons.
- **Build** — `make build` / `make test` / `make package VERSION=0.1.0`.
- **Manual Verification Checklist** — copy spec §10.6.
- **Management API** — copy spec §9 route table.
- **License** — MIT.

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: add README with install, config, privacy, and verification checklist"
```

---

## Task 16: Full integration verification

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `make test`
Expected: all tests PASS across all files.

- [ ] **Step 2: Run vet**

Run: `make vet`
Expected: clean.

- [ ] **Step 3: Build all platforms (matrix sanity)**

Run: `make build GOOS=linux GOARCH=amd64 && make build GOOS=darwin GOARCH=arm64 && make build GOOS=windows GOARCH=amd64`
Expected: three `dist/` artifacts. Clean between builds with `make clean` if needed.

- [ ] **Step 4: Re-run the design simulation (regression)**

Run: `cd sim && go test ./...`
Expected: 231/231 PASS. This confirms no production-code change has invalidated the FSM's design assumptions.

- [ ] **Step 5: Manual checklist dry-run**

Without real OpenAI credentials, load the built DLL into a CPA dev instance, confirm the resource page renders, settings save round-trip works, and the manual-check button produces a log line. Real-credit verification (spec §10.6) happens on first live use — out of scope for this plan.

- [ ] **Step 6: Final commit (if any fixups)**

```bash
git add -A
git commit -m "chore: integration verification complete" || echo "nothing to commit"
git tag v0.1.0
```

---

## Self-Review

**Spec coverage check:**
- §2.1 endpoints → Tasks 1, 5 ✓
- §2.2 headers → Task 5 ✓
- §2.3 credit selection → Tasks 3, 8 ✓
- §3 architecture → Tasks 0–13 (file structure maps 1:1) ✓
- §4 state machine → Task 8 (+ sim regression in Task 16) ✓
- §4.2.1 late-start → covered by sim phase `past_T_before_E_2h`, mirrored in Task 8 step 7 ✓
- §5 retry/idempotency → Tasks 6, 8 ✓
- §6 scheduling → Task 10 ✓
- §6.3 deferral → Task 10 step 5 ✓
- §7 config → Tasks 1, 7 ✓
- §8 logging → Tasks 3 (LogEntry), 9 (ring), 10 (worker logs) ✓
- §9 management API → Task 11 ✓
- §10 testing → every task is TDD; §10.6 manual checklist in Task 15 ✓
- §13 simulation → regression-checked in Task 16 ✓

**Placeholder scan:** The plan contains concrete code in every step. Where a step says "adapt X from sim/edge_test.go," the source file exists and is named precisely. No "TBD" / "implement later."

**Type consistency check:** `State`, `Credit`, `Snapshot`, `ResetAttempt`, `LogEntry` (Task 3); `ConsumeCode`, `ConsumeResponse` (Task 4); `OpenAIClient`/`ResetClient` (Task 5); `Config` (Task 7); `AccountFSM`, `LogFn` (Task 8); `logRing`, `PluginState`, `AccountRuntime` (Task 9); `Worker`, `CredsLoader` (Task 10); `managementHandlers` (Task 11). Method names cross-referenced: `ListCredits`, `GetUsage`, `Consume`, `Step`, `Reset`, `TriggerCheck`, `ForceConfirm`, `EachFSM`, `Logs` — all defined in the task that introduces them.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-18-codex-auto-reset.md`.
