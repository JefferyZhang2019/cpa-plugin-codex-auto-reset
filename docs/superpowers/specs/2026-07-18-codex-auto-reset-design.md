# Codex Auto Reset — Design Spec

- **Date**: 2026-07-18
- **Status**: Approved (pending spec review)
- **Form**: CPA dynamic-library plugin (`.so` / `.dll` / `.dylib`)
- **Repository**: `cpa-plugin-codex-auto-reset`

## 1. Purpose

Codex/ChatGPT accounts accumulate "Reset Bank" credits (rate-limit reset
credits) that a user can manually redeem to reset their weekly rate-limit
window. These credits expire. The author has repeatedly lost credits to
expiry because the redemption is manual and the expiry window is easy to
miss (six credits expired today alone).

This plugin automatically redeems the soonest-expiring available credit
on each enabled Codex account a configurable amount of time before it
would otherwise expire, so no credit is ever lost to forgetfulness.

The plugin is otherwise **independent of CPA's core behavior**. It does
not participate in CPA's account selection, request routing, or circuit
breaking. Its only dependency on CPA is reading account credentials via
the host ABI, plus the standard plugin lifecycle and Management API
surface shared with the reference plugin.

## 2. Reference Materials

The design follows the established patterns of the sibling plugin
`cpa-plugin-codex-quota-scheduler` (ABI lifecycle, Resource/Management
split, bilingual UI, GitHub release workflow).

### 2.1 Endpoint contract — verified from upstream source

The endpoint contract was locked by reading the **official Codex CLI
source** (`openai/codex`, `codex-rs/backend-client`) and its contract
test `rate_limit_resets_tests.rs`, plus `Willxup/cpa-usage-keeper` and
the in-tree `Cli-Proxy-API-Management-Center`. All wire formats below
are copied verbatim from upstream contract tests, not inferred.

**Endpoints (hardcoded Go constants — never user-configurable):**

| Operation | Method | URL |
|---|---|---|
| Query rate-limit usage windows + credit summary | `GET` | `https://chatgpt.com/backend-api/wham/usage` |
| Query detailed reset-credit list | `GET` | `https://chatgpt.com/backend-api/wham/rate-limit-reset-credits` |
| Consume one reset credit | `POST` | `https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume` |

**POST /consume request body** (from upstream
`rate_limit_resets_tests.rs:39-57`, byte-exact):

```json
{
  "redeem_request_id": "<uuid-v4>",
  "credit_id": "<credit-id-from-list-endpoint>"
}
```

`credit_id` is the key safety mechanism: by passing the specific credit
id of the soonest-expiring available credit, OpenAI is told exactly
which credit to consume, eliminating the risk of consuming a later
credit. (`credit_id` may be omitted for "next available" auto-pick;
this plugin always sends it.)

**POST /consume response** (from `types.rs:74-88`):

```json
{
  "code": "reset",
  "credit": { "id": "..." },
  "windows_reset": 2
}
```

`code` is a `snake_case` enum with exactly four values, with the
plugin's handling of each:

| `code` | Meaning | Plugin action |
|---|---|---|
| `"reset"` | Credit consumed | Success → VERIFYING |
| `"already_redeemed"` | Same `redeem_request_id` already succeeded (idempotent) | Success → VERIFYING |
| `"no_credit"` | No available credit (list changed underneath us) | Stop, log, DONE |
| `"nothing_to_reset"` | No eligible window to reset | Stop, log, DONE |

**GET /rate-limit-reset-credits response** (from
`rate_limit_resets_tests.rs:69-94`):

```json
{
  "credits": [
    {
      "id": "credit-1",
      "reset_type": "codex_rate_limits",
      "status": "available",
      "granted_at": "2026-06-17T00:00:00Z",
      "expires_at": "2026-07-17T00:00:00Z",
      "title": "Full reset (Weekly + 5 hr)",
      "description": "Ready to redeem"
    }
  ],
  "available_count": 2,
  "total_earned_count": 4
}
```

The plugin consumes: `credits[].id`, `credits[].status`,
`credits[].expires_at`, and `available_count`. Other returned fields
(`redeem_started_at`, `redeemed_at`, `profile_image_url`,
`profile_user_id`, `total_earned_count`, `title`, `description`) are
accepted but ignored. `expires_at` may be `null` (never expires) — such
credits sort last.

**Idempotency (authoritative, from `codex-rs/app-server/README.md` §8):**
reusing the same `redeem_request_id` returns `already_redeemed`, which
the CLI treats as idempotent success. This is the plugin's retry safety
net.

### 2.2 Headers (hardcoded)

```
Authorization: Bearer <access_token>
Chatgpt-Account-Id: <account_id>
Content-Type: application/json        # POST only
User-Agent: codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal
```

`Oai-Device-Id`, `Oai-Language`, and `Originator` are **not** sent —
verified by grep across the official CLI source (zero matches). They
are not required for these endpoints.

### 2.3 Credit selection algorithm (from official TUI `reset_credits.rs`)

1. Filter to `status == "available"`.
2. Sort by `expires_at` ascending; `null` expiry sorts last (sentinel
   `i64::MAX` in upstream).
3. Take up to `available_count`.
4. The plugin always selects the first entry (soonest expiring) as the
   redeem target and passes its `id` as `credit_id`.

## 3. Architecture

### 3.1 Directory structure

```
cpa-plugin-codex-auto-reset/
├── main.go                 # ABI entry (init/Call/Free/Shutdown)
├── auth.go                 # CodexCredentials + JWT chatgpt_account_id parsing
├── config.go               # User config (3 fields, see §7)
├── constants.go            # Hardcoded endpoints, headers, retry delays, intervals
├── models.go               # State machine models, ResetCredit, Snapshot, LogEntry
├── openai_client.go        # Pure OpenAI HTTP layer (no CPA awareness)
├── quota.go                # Response parsing + credit sort/filter
├── resetbank.go            # State machine engine + worker loop
├── retry.go                # Idempotent-key retry policy
├── state.go                # Disk persistence + log ring
├── management.go           # Management API routes + bilingual HTML page
├── *_test.go               # Tests per file
├── Makefile / build.ps1    # Cross-compile (copied from scheduler)
└── .github/workflows/      # GitHub Actions release (copied from scheduler)
```

### 3.2 Layer separation

- **OpenAI layer** (`openai_client.go`, `quota.go`): pure HTTP and
  parsing. Inputs are `CodexCredentials`; outputs are plain structs.
  No CPA awareness. Independently unit-testable with `httptest.Server`.
- **Management layer** (`resetbank.go`, `retry.go`, `state.go`,
  `management.go`, `config.go`, `models.go`, `constants.go`): state
  machine, scheduling, persistence, routing, HTML rendering.
- **Resource layer** (static HTML/CSS/JS string embedded in
  `management.go`): display only, no business logic, holds no secrets.

### 3.3 Lifecycle

- `cliproxy_plugin_init` → register ABI callbacks, start the
  `ResetBankWorker` goroutine.
- `cliproxy_plugin_shutdown` → `worker.Stop()` for graceful shutdown.
- Credentials are obtained per-account per-check via the host ABI
  callback `ListAuths()` (same mechanism as the scheduler), so the
  plugin always sees the freshest `access_token` CPA holds.

## 4. State Machine

Each enabled account owns one independent state machine instance.

### 4.1 States

| State | Meaning |
|---|---|
| `IDLE` | Steady state. Waiting for next `refresh_interval` check. No credit is near its trigger window. |
| `ARMED` | A credit's trigger window (`expiry − trigger_lead_time`) is within one `refresh_interval` of now, OR has already opened. ARMED performs no polling — it sleeps directly until the trigger time `T`, then transitions to CONFIRMING. This avoids the wasted re-check traffic of an interval-based ARMED while still guaranteeing the trigger-window opener is caught even when the IDLE cadence (e.g. 12h) would otherwise step over it. |
| `CONFIRMING` | The trigger window is open (`now >= expiry − trigger_lead_time`). Performing the pre-reset confirmation GET (verify credit still present, record pre-snapshot). |
| `RESETTING` | POST /consume sent (or mid-retry). Waiting for a terminal response. |
| `VERIFYING` | Reset reported success. After a hardcoded delay, performing post-reset verification GET. |
| `DONE` | Reset cycle finished (success / failure / abandoned). Returns to IDLE. |

### 4.2 Transitions

```
Define:
  trigger_window_opens_at = credit.expires_at - trigger_lead_time

IDLE
  check credits (every refresh_interval):
    - no available credit                          -> IDLE (log "none found")
    - available, but now < trigger_window_opens_at - refresh_interval
                                                   -> IDLE (log "not near window")
    - available, and trigger_window_opens_at is within one refresh_interval
      OR already open (now >= trigger_window_opens_at - refresh_interval)
                                                   -> ARMED

ARMED
  sleep until trigger_window_opens_at (T), no intermediate polling:
    - on wake, now >= T AND credit still available -> CONFIRMING
    - on wake, credit gone / no longer available   -> IDLE

CONFIRMING
  GET credits + GET usage (record pre-snapshot):
    - target credit disappeared (manually used)    -> DONE (abandon, log)
    - target credit still available                -> pick target_credit_id,
                                                       mint redeem_request_id,
                                                       -> RESETTING

RESETTING
  POST /consume {redeem_request_id, credit_id}:
    - code=reset                                   -> VERIFYING
    - code=already_redeemed                        -> VERIFYING (idempotent)
    - code=no_credit / nothing_to_reset            -> DONE (stop, log)
    - HTTP 5xx / network / timeout                 -> retry, same UUID (see §5)
    - HTTP 4xx (other)                             -> DONE (stop, log)
    - retries exhausted                            -> DONE (abandon)

VERIFYING
  after post_reset_verify_delay, GET credits + GET usage (post-snapshot):
    - target gone + count-1 + weekly recovered     -> DONE (success)
    - partial (e.g. count-1 but quota not recovered)-> DONE (warn)
    - mismatch (wrong credit consumed, etc.)       -> DONE (error, halt)

DONE -> IDLE
```

#### 4.2.1 Late-start semantics (verified by simulation)

If the plugin starts (or an account first becomes eligible) at a moment when
`now >= T` (the trigger window is already open) but `now < E` (the credit has
not expired), the first IDLE check transitions straight through ARMED into
CONFIRMING and the reset happens at `now`, not at `T`. The distance from reset
to expiry is therefore **less than** `trigger_lead_time` in this case.

This is correct behavior: the goal is "reset before expiry," not "reset
exactly at T." Simulation phase `past_T_before_E_2h` (and the analogous column
across all 17 parameter groups) confirms exactly one POST /consume happens and
the credit is redeemed before E. The plugin never waits for a future T that is
already in the past.

If `now >= E` at first check, the local `expires_at > now` filter (§2.3 step 1)
excludes the credit entirely; no POST is ever sent (verified by phase
`past_E_5min` across all parameter groups).

### 4.3 Invariants (safety guarantees)

These three invariants directly address the risk of over-consuming or
mis-consuming credits:

1. **Serial per account.** Only one POST /consume is ever in flight per
   account. RESETTING is serial, and all retries reuse the same
   `redeem_request_id`, so the server dedupes. It is physically
   impossible for the plugin to consume two credits on one account in
   one reset cycle.
2. **Always targeted.** The consume body always carries `credit_id`
   pointing at the selected soonest-expiring credit. The plugin never
   uses the "next available" auto-pick form.
3. **Triple deduction check.** Post-reset verification requires all
   three: (a) the target credit id is gone or its status is no longer
   `available`, (b) `available_count` decreased by exactly 1, (c) the
   weekly window recovered. Any mismatch halts immediately and the
   plugin never sends a compensating request.

## 5. Retry & Idempotency

### 5.1 Idempotency key lifecycle

A single `redeem_request_id` (UUID v4) is minted when entering
RESETTING and is reused for every retry within that reset cycle. A new
UUID is minted only at the next logical reset cycle (i.e. after DONE →
IDLE → ... → RESETTING).

```go
type ResetAttempt struct {
    RedeemRequestID string     // UUID v4, stable across retries in this cycle
    TargetCreditID  string     // selected in CONFIRMING
    PreSnapshot     Snapshot   // credits + usage at confirm time
    StartedAt       time.Time
    Attempt         int        // 1-based retry counter
    NextRetryAt     time.Time
}
```

**`already_redeemed` is a success code, not an error.** When a retry reuses a
`redeem_request_id` whose original attempt actually succeeded server-side (the
response was lost), OpenAI returns `code=already_redeemed`. The plugin treats
this identically to `code=reset`: it transitions to VERIFYING and runs the full
post-reset verification. The verification GET will observe the credit as
already consumed and the weekly quota as already refilled (because the prior
attempt did the work server-side), so the triple-deduction check passes
normally. Simulation `TestAlreadyRedeemed_IdempotentSuccess` confirms this path
records exactly one idempotent redemption across two POSTs sharing one UUID.

### 5.2 Retry backoff (hardcoded)

```go
var resetRetryDelays = []time.Duration{
    1 * time.Minute,
    2 * time.Minute,
    5 * time.Minute,
    10 * time.Minute,
    30 * time.Minute,
}
```

At most 5 retries, ~48 min total span. User-configurable: **no**.

### 5.3 Retry trigger conditions

| Response | Retry? |
|---|---|
| `code=reset` | No — success |
| `code=already_redeemed` | No — idempotent success |
| `code=no_credit` | No — list changed; stop |
| `code=nothing_to_reset` | No — stop |
| HTTP 5xx / network / timeout | Yes — same UUID |
| HTTP 4xx (other) | No — stop (avoid hammering bad request) |
| Retries exhausted | Give up; let the credit expire naturally |

### 5.4 Pre-retry re-confirmation

Before each retry's POST, the plugin performs a lightweight
GET /rate-limit-reset-credits:

- If `TargetCreditID` is still in the list and `status == available`:
  the prior attempt genuinely failed → retry POST with the same UUID.
- If `TargetCreditID` is gone or no longer available: the prior attempt
  likely succeeded but the response was lost (or a human used it) →
  transition to VERIFYING, which will determine the truth via GET. The
  plugin never sends a second POST in this branch.

This closes the "network dropped, did it succeed?" window entirely.

### 5.5 Give-up

After 5 failed retries → DONE(abandoned). The plugin does **not** mint a
fresh UUID and try again — doing so could consume a second credit if
the prior UUID's request was actually in flight at the server.

## 6. Scheduling

### 6.1 Per-account time wheel

Each enabled account has its own `nextCheckAt`. The single worker
goroutine computes the minimum `nextCheckAt` across all enabled
accounts and sleeps until then (capped at `refresh_interval`).

- IDLE accounts: `nextCheckAt = now + refresh_interval`.
- ARMED accounts: `nextCheckAt = trigger_window_opens_at` (T). ARMED performs
  no intermediate polling; it sleeps directly to T so the window opener is
  caught exactly on time regardless of where the IDLE cadence landed.
- After any check completes, `nextCheckAt` is recomputed from the
  completion time.

**Worked example** (defaults 12h / 6h): a credit expires at 2026-07-19
18:00. Its trigger window opens at 2026-07-19 12:00 (= 18:00 − 6h).
IDLE checks every 12h. If a check at 2026-07-19 03:00 sees the credit,
`trigger_window_opens_at - refresh_interval = 2026-07-19 00:00`, and
`now (03:00) >= 00:00`, so the account enters ARMED even though the
window is not yet open. ARMED then re-checks every 15 min; at 12:00 the
window is open and CONFIRMING begins. Without ARMED, the next IDLE
check would be 2026-07-19 15:00 — only 3h before expiry, and worse, a
check at 2026-07-19 06:00 followed by 2026-07-19 18:00 would arrive
exactly at expiry with no margin. ARMED guarantees the window opener is
always caught within 15 minutes regardless of where the IDLE cadence
happens to land.

### 6.2 Credential sync (real-time)

Before each per-account check, the worker calls the host ABI
`ListAuths()` to fetch the freshest credential for that account. This
guarantees the plugin uses the current `access_token` even if CPA just
refreshed it. Manual buttons use the same channel.

### 6.3 Manual triggers and deferral

- **Manual check button** (per account or all): sets
  `nextCheckAt = now` and wakes the worker.
- **Deferral rule**: after a manual check completes, `nextCheckAt`
  becomes `completion_time + refresh_interval` (not the original
  scheduled time). A manual check is treated as "consuming" that
  cycle's slot, so the next automatic check is 12h from now, not 12h
  from the original schedule.
- **Manual reset button** (per account): forces the state machine into
  CONFIRMING, but still runs the full confirmation + idempotent POST +
  verification flow. No safety check is bypassed.

## 7. Configuration

### 7.1 User-configurable fields (3 only)

```yaml
refresh_interval: 12h       # IDLE re-check interval
trigger_lead_time: 6h       # redeem this long before a credit expires
enabled_accounts:           # subset of Codex accounts (auth_index list)
  - auth_index_1
  - auth_index_2
```

These are editable in the UI and persisted to the plugin state file
behind the Management key. Empty `enabled_accounts` means the plugin
does nothing — equivalent to a soft disable. There is no separate
`handle_enabled` master switch; plugin enable/disable is CPA's job via
`plugins.configs.codex-auto-reset.enabled`.

### 7.2 Hardcoded constants (not configurable)

These live in `constants.go` and are never exposed via config or UI:

- All three OpenAI endpoint URLs (§2.1).
- Request headers and User-Agent (§2.2).
- `post_reset_verify_delay = 1 * time.Minute`.
- `reset_retry_delays = [1m, 2m, 5m, 10m, 30m]`.
- `max_log_entries = 200`.
- `log_retention = 24h`.
- Credit selection algorithm (§2.3).
- ARMED sleep-to-T behavior (no polling constant — §4.1).

## 8. Logging

### 8.1 Log entry structure

```go
type LogEntry struct {
    Timestamp  time.Time
    Level      string              // info / warn / error
    Scope      string              // "system" or auth_id (per account)
    State      string              // current FSM state
    Message    string              // what the last cycle did
    NextAction string              // what the next cycle will do (optional)
    NextAt     *time.Time          // when (optional; for UI countdown)
    Details    map[string]any      // snapshots, credit info
}
```

### 8.2 Example logs

**A — Routine patrol, nothing imminent (IDLE):**
```
[2026-07-18 14:00:00] [INFO] [account:alice@x.com] [IDLE]
  Last: Checked Reset Bank. 2 credits available; earliest expires 2026-08-15
        (28 days away). Nothing imminent.
  Next: Will re-check at 2026-07-19 02:00:00 (in ~12h).
```

**B — Credit imminent (IDLE → ARMED):**
```
[2026-07-18 14:00:00] [WARN] [account:bob@y.com] [ARMED]
  Last: Found credit credit-xyz expiring in 4h (<= trigger_lead_time 6h). Armed.
  Next: Will re-check at 2026-07-18 14:15:00 (in ~15m).
        If the credit is still present, a reset will be triggered.
```

**C — Reset issued (CONFIRMING → RESETTING):**
```
[2026-07-18 14:15:00] [INFO] [account:bob@y.com] [RESETTING]
  Last: Re-confirmed credit credit-xyz available (expires 2026-07-18 18:00).
        Pre-snapshot: available_count=2, weekly remaining=15%.
        Selected target_credit_id=credit-xyz, redeem_request_id=a1b2...
        Sent POST /consume.
  Next: Awaiting response (retry 0/5).
```

**D — Reset succeeded (VERIFYING → DONE):**
```
[2026-07-18 14:16:00] [INFO] [account:bob@y.com] [DONE]
  Last: Reset succeeded.
        POST response: code=reset, windows_reset=2.
        Before: available_count=2, weekly=15%, credit credit-xyz=available.
        After:  available_count=1, weekly=100%, credit credit-xyz gone.
        Checks: target gone ✓, count -1 ✓, quota recovered ✓.
  Next: Will resume routine patrol at 2026-07-19 02:15:00 (in ~12h).
```

**E — Deduction anomaly (VERIFYING → DONE, error):**
```
[2026-07-18 14:16:00] [ERROR] [account:bob@y.com] [DONE]
  Last: Verification mismatch; halted.
        Expected: credit credit-xyz consumed.
        Actual:   credit credit-xyz still present; credit credit-abc
                  (expires later) is gone instead.
        Judgement: OpenAI consumed a non-target credit. Halting to avoid
                   further mis-consumption. No compensating request sent.
        redeem_request_id=a1b2... (retained for diagnosis).
  Next: Will resume routine patrol at 2026-07-19 02:15:00.
        The credit will expire naturally.
```

**F — System startup:**
```
[2026-07-18 14:00:00] [INFO] [system] [-]
  Last: Plugin started. 3 enabled accounts (alice@, bob@, carol@). State
        restored from disk.
  Next: First patrol at 2026-07-18 14:00:05.
```

### 8.3 UI

Bilingual (English / Chinese) resource page, browser-language detection
plus a manual switcher (same UX as the scheduler plugin). Per account,
a card shows: current FSM state badge, Reset Bank credit list with the
target credit highlighted, weekly quota, last-cycle summary, next-cycle
plan with a live countdown, and action buttons (manual check, manual
reset, history). A system area at the top shows global status and the
next system-level action time.

## 9. Management API

```
GET  /v0/resource/plugins/codex-auto-reset/status            # resource page (read-only HTML)
GET  /v0/management/plugins/codex-auto-reset/status?format=json
GET  /v0/management/plugins/codex-auto-reset/logs
PUT  /v0/management/plugins/codex-auto-reset/settings
POST /v0/management/plugins/codex-auto-reset/check            # manual check; optional auth_index
POST /v0/management/plugins/codex-auto-reset/check/all
POST /v0/management/plugins/codex-auto-reset/reset            # manual reset; requires auth_index
GET  /v0/management/plugins/codex-auto-reset/export
POST /v0/management/plugins/codex-auto-reset/import
```

All `/v0/management/...` routes require the CPA Management key, same
security boundary as the scheduler. The resource page never persists
the key beyond the browser page session and never writes it to logs,
exports, `localStorage`, or `sessionStorage`. The Management key never
leaves the browser session.

## 10. Testing

End-to-end testing against live OpenAI is hard (credits are scarce and
the interval between real opportunities is long), so test effort
concentrates on logic that can be unit-tested, with a manual checklist
for the live path.

### 10.1 OpenAI layer unit tests (no network)

- `quota_test.go`: parse the upstream contract-test fixture (§2.1
  JSON); sort credits by `expires_at` ascending; `null` sorts last;
  filter out `status != "available"`.
- `openai_client_test.go`: `httptest.Server` mock asserts the GET/POST
  URL, headers, and body match the upstream contract byte-for-byte,
  including `{"redeem_request_id":..., "credit_id":...}` with
  `credit_id` omitted when absent (never `null`).

### 10.2 State machine unit tests (no network)

A fake OpenAI client drives every transition path:
- IDLE → ARMED → CONFIRMING → RESETTING(reset) → VERIFYING → DONE(success).
- IDLE → ARMED → CONFIRMING → credit vanished → DONE(abandon).
- RESETTING network error → retries 1/2/5/10/30 → DONE(give up).
- RESETTING → already_redeemed → VERIFYING(treated as success).
- VERIFYING consumed wrong credit → DONE(error, halt).
- Concurrency invariant: two goroutines trigger one account; assert
  exactly one POST /consume was sent.

### 10.3 Integration test

`httptest.Server` emulates all three OpenAI endpoints; the worker runs
several cycles end-to-end. Asserts log format, state transitions, and
disk-persistence recovery across a simulated restart.

### 10.4 Retry unit tests

Backoff sequence; pre-retry re-confirmation GET logic; per-response-code
branching; UUID reuse across retries within one cycle; new UUID across
cycles.

### 10.5 Config unit tests

Defaults, normalization, YAML parsing, empty `enabled_accounts`
behavior (mirrors the scheduler's `config_test.go` style).

### 10.6 Manual verification checklist (in README)

- [ ] GET list reads credits correctly.
- [ ] Real POST /consume consumes the targeted credit.
- [ ] `already_redeemed` idempotent behavior with a reused UUID.
- [ ] UI bilingual switching.
- [ ] Manual check and manual reset buttons.
- [ ] Deferral: after a manual check, next automatic check is
      `refresh_interval` from the manual check's completion time.

## 11. Open Questions

None at spec time. The three runtime concerns the author raised — (a)
ensure only one request consumes one credit, (b) ensure the consumed
credit is the soonest-expiring one, (c) stop on anomaly — are covered
by invariants §4.3 and the pre-retry re-confirmation §5.4.

## 12. Out of Scope

- CPA account selection, routing, circuit breaking (the scheduler
  plugin's domain).
- Notifications (webhook/email) on reset events. May be added later.
- Auto-rotating redeem behavior across multiple credits in one cycle.
  One credit per cycle, then back to IDLE.
- Idempotency-key TTL discovery. The server-side dedup window length is
  not publicly documented; the plugin assumes it is long enough that a
  48-minute retry span is safe. If experience shows otherwise, this is
  a `constants.go` change, not a design change.

## 13. Simulation Validation

The state machine in §4 was validated with a deterministic event-driven
simulator before any production code was written. The simulator lives in
`sim/` (separate `go.mod`; not part of the plugin's build) and is kept as
design evidence and a regression harness for future FSM changes.

**Coverage:** 17 parameter groups (integer-multiple, non-integer-multiple,
coprime, sub-hour) × 13 start phases (far-before-M, M±7s, ARMED-window
interior, T−3min, T-exact, past-T, past-E, plus patrol-straddle phases that
exercise non-aligned IDLE cadence) = **221 matrix subtests**, plus **10
targeted edge-case tests** (multi-credit targeting, idempotent retry,
already_redeemed, retries-exhausted, no_credit stop, credit-vanishes-during-
ARMED, wrong-credit-consumed detection, past-expiry no-reset, long patrol
rhythm). All 231 pass.

**What the simulation proved:**
- Every (R, L, start-phase) combination redeems exactly one credit before E.
- Reset lands within 1 second of T whenever the plugin starts before T.
- ARMED performs zero extra GET requests during its sleep to T.
- Phase offsets of 7 seconds, R/2, and 5R+17min are all absorbed.
- Late start (now ≥ T) resets at `now` with distance-to-E < L (§4.2.1).
- Wrong-credit consumption is detected by the triple-deduction check and
  halts without sending a compensating request.
- Idempotent retry with a shared UUID never double-consumes.

**What the simulation does NOT cover** (must be validated manually per §10.6):
real network behavior against OpenAI, real ChatGPT OAuth token refresh, the
CPA host ABI surface, and the bilingual UI. The simulator is a logic
validator, not an integration test.
