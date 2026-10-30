# Codex Auto Reset

`codex-auto-reset` is a CLIProxyAPI (CPA) dynamic library plugin that
automatically redeems the soonest-expiring Codex "Reset Bank" credit on each
enabled account before it expires, so no credit is ever lost to forgetfulness.

Codex/ChatGPT accounts accumulate rate-limit reset credits that a user can
manually redeem to reset their weekly rate-limit window. These credits expire,
and the expiry window is easy to miss. This plugin watches each enabled
account and redeems the credit a configurable amount of time before it would
otherwise expire.

The plugin is otherwise **independent of CPA's core behavior**. It does not
participate in CPA's account selection, request routing, or circuit breaking.
Its only dependency on CPA is reading account credentials via the host ABI,
plus the standard plugin lifecycle and Management API surface.

## How It Works

For each enabled Codex account, the plugin runs a 6-state state machine:

```
IDLE       → patrol every refresh_interval; if the soonest-expiring credit's
             trigger window is within one interval, pin its ID and enter ARMED
ARMED      → sleep directly to the trigger time T = expiry − trigger_lead_time
             (no polling); on wake, enter CONFIRMING
CONFIRMING → re-list credits; validate the pinned target is still available,
             unexpired, and due; if early, re-arm the same ID at its fresh T;
             otherwise fetch usage, mint a redeem_request_id, enter RESETTING
RESETTING  → POST /consume with {redeem_request_id, credit_id}; on success
             enter VERIFYING; on transient error retry (same idempotency key,
             1/2/5/10/30 min backoff, max 5); on 4xx or no_credit stop
VERIFYING  → after 1 min, re-list + fetch usage; triple deduction check
             (target gone + count −1 + weekly recovered); enter DONE
DONE       → cycle complete; back to IDLE
```

An automatic cycle never substitutes another credit if its pinned target
is redeemed, removed, or expired, even if another credit is already due. A
missing target ID also abandons the cycle. Credits with no expiry are never
automatically redeemed; if the pinned target becomes non-expiring, that cycle
is abandoned. The next IDLE patrol can schedule a new cycle normally.

**Check** (including Check All) pulls only IDLE accounts to an immediate
patrol; it never authorizes an early reset. Accounts inside an active cycle
(armed, retrying, verifying) keep their schedule — confirmation still
rechecks the target's current expiry and waits until its updated trigger
time, so an early wake cannot bypass it. **Reset** explicitly requests immediate manual
confirmation, clears the old automatic target, and selects the currently
soonest-expiring available credit, even far before its trigger time (or with
no expiry). It still runs the full confirm, consume, and verify flow. Repeated
Reset requests during consume/retry or verification leave that active attempt
intact. Returning to IDLE clears both the pinned target and manual intent.

**Safety invariants** (covered by production tests and the simulation in `sim/`):

1. **Serial per account.** Only one POST /consume is ever in flight per
   account per cycle. All retries reuse the same target and
   `redeem_request_id`, allowing OpenAI to deduplicate them.
2. **Always targeted.** The consume body always carries `credit_id`. Automatic
   confirmation validates the ID pinned when arming, not a newly selected
   credit. Manual Reset explicitly selects a fresh available target.
3. **Triple deduction check.** Verification requires (a) the target credit is
   gone, (b) `available_count` decreased by exactly 1, AND (c) weekly quota
   recovered. Any mismatch halts immediately — no compensating request is sent.

## Privacy And Data Disclosure

This plugin runs inside CPA and uses CPA-provided host callbacks plus
plugin-owned CPA Management API routes. The browser resource page is only the
UI surface. State-changing actions require the CPA Management key and are sent
to `/v0/management/plugins/codex-auto-reset/...`. The page keeps that key only
in the current browser page session and does not write it to plugin state,
exports, logs, `localStorage`, or `sessionStorage`. The plugin does not run an
external service and does not send data to the plugin author.

The plugin sends authenticated requests to the ChatGPT backend to read quota
state and consume reset credits:

```text
GET  https://chatgpt.com/backend-api/wham/usage
GET  https://chatgpt.com/backend-api/wham/rate-limit-reset-credits
POST https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume
```

These requests use the Codex credentials already configured in CPA. The plugin
uses the responses to calculate reset-credit expiry, target the soonest-expiring
credit, and verify the reset took effect.

After a reset is verified, the plugin also asks CPA's host API to clear that
credential's core routing cooldown (`host.routing.reset_cooldown`). Redeeming a
credit resets the upstream windows, but CPA may still hold the credential in
cooldown until the old 429's `next_retry_after`; without this step the account
looks recovered in the UI while every request keeps failing at the router. The
call is best-effort: older hosts that do not expose the callback log a warning
and the verified reset still stands.

### State Storage And Durability

The plugin persists its state file (`state.json`) — the user config fields,
per-account FSM state, reset history, and recent log entries — at the first
available location below (resolved once at startup, with one-time forward
migration from older locations; superseded files are kept as `.migrated`):

1. `state_dir` from the plugin's `config_yaml` block (operator override,
   must be an absolute path)
2. `<CPA auth dir>/plugin-state/codex-auto-reset/state.json` — derived from
   the auth file paths reported by the host. **Recommended and default**: in
   container deployments the auth dir is the tree operators volume-mount, so
   state survives CPA image updates and container recreates.
3. `<OS user config dir>/CLIProxyAPI/codex-auto-reset/state.json` — fallback
   (Windows `%APPDATA%`, Linux `~/.config`). **Ephemeral inside containers**
   unless that path is volume-mounted.

The plugin also keeps a one-generation backup (`state.json.bak`, refreshed on
every successful load) and, if the main file is ever corrupt, quarantines the
original bytes as `state.json.corrupt-<timestamp>` before recovering from the
backup — state is never silently destroyed.

Docker note: with the default auth-dir location, mounting your auth dir (you
already must, for credentials) is enough, e.g.
`-v ./cpa/auths:/root/.cli-proxy-api`. No access tokens or credential
material are persisted in the state file.

## Installation

Download the zip for your platform from the latest GitHub release:

```text
codex-auto-reset_<version>_<goos>_<goarch>.zip
```

Extract the dynamic library and place it in CPA's plugin directory for your
platform. The zip contains the library at the archive root:

- macOS: `codex-auto-reset.dylib`
- Linux: `codex-auto-reset.so`
- Windows: `codex-auto-reset.dll`

Example:

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/darwin/arm64
cp codex-auto-reset.dylib /path/to/CLIProxyAPI/plugins/darwin/arm64/
```

## CPA Configuration

Enable global plugins and this plugin in CPA:

```yaml
plugins:
  enabled: true
  configs:
    codex-auto-reset:
      enabled: true
      priority: 1
```

The plugin reads its 3 user-tunable fields from `config_yaml`:

```yaml
refresh_interval: 12h        # IDLE patrol cadence (default 12h)
trigger_lead_time: 6h        # redeem this long before a credit expires (default 6h)
enabled_accounts:            # default: ["*"] — every Codex account CPA knows,
  - auth_index_1             # including accounts added after startup. Replace
  - auth_index_2             # with explicit auth_indexes to opt a subset in;
state_dir: /absolute/path    # `enabled_accounts: []` explicitly disables all.
                             # state_dir is an optional operator override (see
                             # State Storage And Durability)
```

**Fresh installs work with zero setup**: `enabled_accounts` defaults to the
all-accounts wildcard `["*"]`, so the plugin immediately monitors every Codex
account in CPA — no management key, no UI visit, no manual account selection.
The wildcard is re-resolved from the host about once a minute, so accounts
logged in later are picked up (and removed accounts dropped) automatically.
Existing installations keep their saved `enabled_accounts` as-is after
upgrading; only a brand-new state file gets the wildcard default.

All other behavior (OpenAI endpoints, retry backoff, post-reset verify delay,
log retention) is hardcoded and not user-configurable.

## Management UI

Open the resource page from CPA's plugin resources, or visit:

```text
/v0/resource/plugins/codex-auto-reset/status
```

The page derives its Management API root from the loaded page URL, so it also
works when CPA is exposed under a reverse-proxy path prefix (for example
`/cpa/v0/resource/plugins/codex-auto-reset/status` calls
`/cpa/v0/management/plugins/codex-auto-reset/...`). Root deployments yield an
empty prefix and keep using `/v0/management/...`.

The page follows the browser language by default and can be switched between
English and Chinese manually. It provides:

- The 3 user config fields (editable, saved via the Management key). The
  enabled-accounts section has an **All accounts (automatic)** toggle — on by
  default for fresh installs — that covers every Codex account CPA knows,
  including future ones; uncheck it to pick accounts individually.
- Per-account cards showing FSM state, credits available, weekly remaining,
  and manual Check / Reset buttons.
- A live log view (last 100 entries).
- A live countdown to each account's next scheduled action.

The resource page asks for the CPA Management key before it performs protected
actions. This follows CPA's security boundary: `/v0/resource/plugins/...`
serves the browser resource page, while `/v0/management/...` handles
authenticated management operations.

## Management API

```
GET  /v0/resource/plugins/codex-auto-reset/status            # resource page (read-only HTML)
GET  /v0/management/plugins/codex-auto-reset/status          # JSON status (config + accounts)
GET  /v0/management/plugins/codex-auto-reset/logs            # recent log entries
PUT  /v0/management/plugins/codex-auto-reset/settings        # update the 3 config fields
POST /v0/management/plugins/codex-auto-reset/check           # manual check; body {auth_index}
POST /v0/management/plugins/codex-auto-reset/check/all       # manual check all accounts
POST /v0/management/plugins/codex-auto-reset/reset           # manual reset; body {auth_index}
GET  /v0/management/plugins/codex-auto-reset/export          # export state JSON
POST /v0/management/plugins/codex-auto-reset/import          # import state JSON
```

All `/v0/management/...` routes require the CPA Management key.

## Build

Requirements:

- Go 1.26 or newer, as declared by `go.mod`.
- CGO support.
- A C compiler for `-buildmode=c-shared` (MinGW-w64 on Windows, gcc/clang on
  Linux/macOS).
- `make` for the cross-platform release workflow (optional — direct `go
  build` works too).

Run tests:

```bash
make test
```

Build the dynamic library for the current platform:

```bash
make build
```

Build and package the release zip:

```bash
make package VERSION=0.1.0
```

Windows users can also use the PowerShell helper:

```powershell
.\build.ps1
```

The PowerShell script builds `dist/codex-auto-reset.dll` and requires a C
compiler such as MinGW-w64 on `PATH`.

## GitHub Releases

GitHub Actions builds release assets when a tag matching `v*` is pushed. Use
a dotted numeric version tag such as:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Build` workflow runs tests and creates the release automatically. Release
assets are named:

```text
codex-auto-reset_<version>_<goos>_<goarch>.zip
checksums.txt
```

## Manual Verification Checklist

Before relying on this plugin in production, verify against real OpenAI:

- [ ] The resource page renders and the language switcher works.
- [ ] Saving settings round-trips through the state file.
- [ ] The manual Check button produces a log line for the target account.
- [ ] A real GET to `/rate-limit-reset-credits` returns the credit list.
- [ ] A real POST to `/consume` with `{redeem_request_id, credit_id}` returns
      `code=reset` and the target credit disappears.
- [ ] Reusing the same `redeem_request_id` returns `code=already_redeemed`
      (idempotent success).
- [ ] After a successful reset, the weekly quota recovers to 100%.
- [ ] Deferral: after a manual Check in IDLE with no imminent credit, the next
      patrol is `refresh_interval` from the manual check's completion time.
- [ ] Check while ARMED neither pulls the trigger wake forward nor consumes
      before the pinned target's trigger time.
- [ ] Redeeming the pinned target externally while ARMED abandons that cycle
      without consuming a different credit.

Real-credit verification is necessarily infrequent (credits are scarce and
expire over weeks). The `sim/` directory contains a deterministic event-driven
simulation that validated the FSM design across 231 tests before any production
code was written; run `cd sim && go test ./...` to re-verify the design.

## License

MIT License. See [LICENSE](LICENSE).
