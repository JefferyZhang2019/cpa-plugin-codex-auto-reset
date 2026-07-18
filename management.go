package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const pluginID = "codex-auto-reset"

// AccountLister returns the Codex accounts discoverable through CPA (via the
// host.auth.list ABI callback). Injected from main.go so management.go has no
// direct C/ABI dependency. Returns one entry per Codex auth file, sorted.
type AccountLister func() ([]AccountOption, error)

// AccountOption is one selectable Codex account for the UI's checkbox list.
type AccountOption struct {
	AuthIndex string `json:"auth_index"`
	ID        string `json:"id,omitempty"`  // credential ID / file name (e.g. codex-44411af1-email-team.json)
	Name      string `json:"name"`          // human-readable display label (id, then name, then email, then auth_index)
	Email     string `json:"email,omitempty"`
	Account   string `json:"account,omitempty"` // ChatGPT account ID
	Enabled   bool   `json:"enabled"`           // true if currently in EnabledAccounts
}

// managementHandlers owns the plugin's mutable state and serves the JSON
// routes from spec §9. The HTML resource page (Task 12) is rendered by
// renderStatusHTML, called from the resource/status route.
type managementHandlers struct {
	mu        sync.Mutex
	state     *PluginState
	statePath string
	worker    *Worker
	lister    AccountLister        // may be nil if host ABI not wired (tests)
	OnSettingsChanged func()       // injected by main.go; restarts the worker with new config
}

func newManagementHandlers(state *PluginState, statePath string, worker *Worker) *managementHandlers {
	return &managementHandlers{state: state, statePath: statePath, worker: worker}
}

// resourceBasePath is the URL prefix under which CPA serves browser-navigable
// plugin resources. Requests whose Path starts with this prefix are resource
// loads (HTML page); all others are Management API calls (JSON).
const resourceBasePath = "/v0/resource/plugins/" + pluginID

// isResourcePath reports whether the raw request path targets the browser-
// navigable resource surface (returns HTML) rather than the Management API
// (returns JSON). Pattern matches the scheduler plugin's isResourcePath.
func isResourcePath(path string) bool {
	return path == resourceBasePath || strings.HasPrefix(path, resourceBasePath+"/")
}

// handle dispatches one management or resource request. rawPath is the
// original CPA request path (e.g. /v0/management/plugins/codex-auto-reset/
// status or /v0/resource/plugins/codex-auto-reset/status); it is used to
// distinguish HTML (resource) from JSON (management) responses for /status.
func (h *managementHandlers) handle(method, rawPath string, headers http.Header, body []byte) (int, []byte, string) {
	path := normalizePath(rawPath)
	switch {
	case method == http.MethodGet && path == "/status":
		// Same path serves HTML (resource load) or JSON (management API).
		if isResourcePath(rawPath) {
			return http.StatusOK, []byte(renderStatusHTML(h.state)), "text/html; charset=utf-8"
		}
		st, b := h.statusJSON()
		return st, b, "application/json"
	case method == http.MethodGet && path == "/accounts":
		st, b := h.accountsJSON()
		return st, b, "application/json"
	case method == http.MethodGet && path == "/logs":
		st, b := h.logsJSON()
		return st, b, "application/json"
	case method == http.MethodPut && path == "/settings":
		st, b := h.putSettings(body)
		return st, b, "application/json"
	case method == http.MethodPost && path == "/check":
		st, b := h.checkOne(body)
		return st, b, "application/json"
	case method == http.MethodPost && path == "/check/all":
		if h.worker != nil {
			h.worker.TriggerCheck("")
		}
		st, b := jsonOK(map[string]any{"triggered": "all"})
		return st, b, "application/json"
	case method == http.MethodPost && path == "/reset":
		st, b := h.resetOne(body)
		return st, b, "application/json"
	case method == http.MethodGet && path == "/export":
		st, b := h.exportState()
		return st, b, "application/json"
	case method == http.MethodPost && path == "/import":
		st, b := h.importState(body)
		return st, b, "application/json"
	}
	st, b := jsonStatus(http.StatusNotFound, map[string]any{"error": "route not found"})
	return st, b, "application/json"
}

// normalizePath strips every CPA-known prefix from a management or resource
// path so dispatch can switch on a canonical suffix (e.g. "/status",
// "/settings"). Both /v0/management/plugins/codex-auto-reset/status and
// /v0/resource/plugins/codex-auto-reset/status normalize to /status.
// A bare "/codex-auto-reset/status" (without the /v0/... prefix) is also
// accepted for resilience.
func normalizePath(path string) string {
	for _, prefix := range []string{
		"/v0/management/plugins/" + pluginID,
		"/v0/resource/plugins/" + pluginID,
		"/plugins/" + pluginID,
		"/" + pluginID,
		pluginID,
	} {
		if strings.HasPrefix(path, prefix) {
			stripped := strings.TrimPrefix(path, prefix)
			if stripped == "" || stripped == "/" {
				return "/"
			}
			return stripped
		}
	}
	return path
}

func (h *managementHandlers) statusJSON() (int, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Deep-copy accounts so the JSON encoder doesn't race with the worker.
	accts := make(map[string]AccountRuntime, len(h.state.Accounts))
	for k, v := range h.state.Accounts {
		accts[k] = v
	}
	out := map[string]any{
		"plugin":   pluginID,
		"config":   h.state.Config,
		"accounts": accts,
	}
	return jsonOK(out)
}

// accountsJSON returns the list of Codex accounts CPA knows about, annotated
// with whether each is currently enabled. The UI renders these as checkboxes.
func (h *managementHandlers) accountsJSON() (int, []byte) {
	if h.lister == nil {
		return jsonStatus(http.StatusServiceUnavailable, map[string]any{"error": "account listing unavailable (host not wired)"})
	}
	opts, err := h.lister()
	if err != nil {
		return jsonStatus(http.StatusBadGateway, map[string]any{"error": "list accounts: " + err.Error()})
	}
	// Mark enabled ones.
	h.mu.Lock()
	enabled := make(map[string]bool, len(h.state.Config.EnabledAccounts))
	for _, id := range h.state.Config.EnabledAccounts {
		enabled[id] = true
	}
	h.mu.Unlock()
	for i := range opts {
		opts[i].Enabled = enabled[opts[i].AuthIndex]
	}
	return jsonOK(map[string]any{"accounts": opts})
}

func (h *managementHandlers) putSettings(body []byte) (int, []byte) {
	var req struct {
		RefreshInterval string   `json:"refresh_interval"`
		TriggerLeadTime string   `json:"trigger_lead_time"`
		EnabledAccounts []string `json:"enabled_accounts"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		// Maybe it's YAML.
		parsed, err2 := parseConfigYAML(body)
		if err2 != nil {
			return jsonStatus(http.StatusBadRequest, map[string]any{"error": "invalid settings: " + err.Error()})
		}
		req.RefreshInterval = parsed.RefreshInterval.String()
		req.TriggerLeadTime = parsed.TriggerLeadTime.String()
		req.EnabledAccounts = parsed.EnabledAccounts
	}
	cfg := Config{EnabledAccounts: req.EnabledAccounts}
	if req.RefreshInterval != "" {
		d, err := time.ParseDuration(req.RefreshInterval)
		if err != nil {
			return jsonStatus(http.StatusBadRequest, map[string]any{"error": "invalid refresh_interval: " + err.Error()})
		}
		cfg.RefreshInterval = d
	}
	if req.TriggerLeadTime != "" {
		d, err := time.ParseDuration(req.TriggerLeadTime)
		if err != nil {
			return jsonStatus(http.StatusBadRequest, map[string]any{"error": "invalid trigger_lead_time: " + err.Error()})
		}
		cfg.TriggerLeadTime = d
	}
	cfg = normalizeConfig(cfg)
	h.mu.Lock()
	h.state.Config = cfg
	h.mu.Unlock()
	if err := saveState(h.statePath, h.state); err != nil {
		return jsonStatus(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	// Restart the worker so the new config (especially enabled_accounts and
	// refresh_interval) takes effect immediately. Without this the worker
	// keeps running with the old config until a plugin reload.
	if h.OnSettingsChanged != nil {
		h.OnSettingsChanged()
	}
	return jsonOK(map[string]any{
		"refresh_interval": formatDuration(cfg.RefreshInterval),
		"trigger_lead_time": formatDuration(cfg.TriggerLeadTime),
		"enabled_accounts":  cfg.EnabledAccounts,
	})
}

func (h *managementHandlers) logsJSON() (int, []byte) {
	var entries []LogEntry
	if h.worker != nil {
		entries = h.worker.Logs().all()
	}
	return jsonOK(map[string]any{"entries": entries})
}

func (h *managementHandlers) checkOne(body []byte) (int, []byte) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(body, &req)
	if h.worker != nil {
		h.worker.TriggerCheck(req.AuthIndex)
	}
	return jsonOK(map[string]any{"triggered": req.AuthIndex})
}

func (h *managementHandlers) resetOne(body []byte) (int, []byte) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
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
	if s.Accounts == nil {
		s.Accounts = map[string]AccountRuntime{}
	}
	h.mu.Lock()
	h.state.Config = s.Config
	h.state.Accounts = s.Accounts
	h.mu.Unlock()
	if err := saveState(h.statePath, h.state); err != nil {
		return jsonStatus(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return jsonOK(map[string]any{"imported": true})
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

// renderStatusHTML returns the bilingual resource page. The page is static
// HTML with inline CSS and JS; it fetches /v0/management/plugins/codex-
// auto-reset/status and /logs at runtime using a user-supplied CPA Management
// key. No secrets are embedded in the HTML.
func renderStatusHTML(state *PluginState) string {
	_ = state
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Codex Auto Reset</title>
  <style>
    :root {
      color-scheme: light dark;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
      background: Canvas;
      color: CanvasText;
    }
    * { box-sizing: border-box; }
    body { margin: 0; }
    main { max-width: 1100px; margin: 0 auto; padding: 24px; }
    header { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 18px; flex-wrap: wrap; }
    h1 { margin: 0; font-size: 22px; font-weight: 700; }
    h2 { margin: 0 0 12px; font-size: 15px; font-weight: 700; }
    label { display: grid; gap: 6px; font-size: 13px; font-weight: 600; }
    input, select, button, textarea { font: inherit; }
    input, select, textarea {
      width: 100%; border: 1px solid color-mix(in srgb, CanvasText 18%, Canvas 82%);
      border-radius: 6px; padding: 8px 10px; background: Canvas; color: CanvasText;
    }
    button {
      border: 0; border-radius: 6px; padding: 8px 12px;
      background: #0f766e; color: #fff; font-weight: 700; cursor: pointer;
    }
    button.secondary { background: color-mix(in srgb, CanvasText 10%, Canvas 90%); color: CanvasText; }
    button:disabled { opacity: .5; cursor: not-allowed; }
    .layout { display: grid; grid-template-columns: 320px minmax(0,1fr); gap: 16px; align-items: start; }
    .panel {
      border: 1px solid color-mix(in srgb, CanvasText 14%, Canvas 86%);
      border-radius: 8px; padding: 16px;
      background: color-mix(in srgb, Canvas 96%, CanvasText 4%);
    }
    .fields { display: grid; gap: 12px; }
    .actions { display: flex; gap: 8px; flex-wrap: wrap; }
    .actions button { width: auto; }
    .card {
      border: 1px solid color-mix(in srgb, CanvasText 14%, Canvas 86%);
      border-radius: 8px; padding: 14px; margin-bottom: 12px;
      background: color-mix(in srgb, Canvas 96%, CanvasText 4%);
    }
    .card-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
    .auth-id { font-weight: 700; font-size: 14px; }
    .state-badge {
      font-size: 11px; font-weight: 700; padding: 3px 8px; border-radius: 4px;
      background: color-mix(in srgb, #2563eb 18%, Canvas 82%); color: color-mix(in srgb, #2563eb 80%, CanvasText 20%);
    }
    .state-badge.ARMED, .state-badge.CONFIRMING, .state-badge.RESETTING, .state-badge.VERIFYING {
      background: color-mix(in srgb, #b45309 18%, Canvas 82%); color: color-mix(in srgb, #b45309 80%, CanvasText 20%);
    }
    .state-badge.DONE { background: color-mix(in srgb, #15803d 18%, Canvas 82%); color: color-mix(in srgb, #15803d 80%, CanvasText 20%); }
    .kv { display: grid; grid-template-columns: auto 1fr; gap: 4px 12px; font-size: 12px; }
    .kv .k { color: color-mix(in srgb, CanvasText 60%, Canvas 40%); }
    .muted { color: color-mix(in srgb, CanvasText 60%, Canvas 40%); font-size: 12px; }
    .next { margin-top: 8px; font-size: 12px; padding: 8px; border-radius: 6px; background: color-mix(in srgb, #2563eb 8%, Canvas 92%); }
    .countdown { color: color-mix(in srgb, CanvasText 60%, Canvas 40%); font-weight: 600; }
    .log { font-family: ui-monospace, "Cascadia Code", Consolas, monospace; font-size: 11px; line-height: 1.5; max-height: 320px; overflow-y: auto; padding: 8px; border-radius: 6px; background: color-mix(in srgb, CanvasText 4%, Canvas 96%); }
    .log .line { white-space: pre-wrap; word-break: break-word; }
    .log .ts { color: color-mix(in srgb, CanvasText 55%, Canvas 45%); }
    .log .lvl-warn { color: #b45309; }
    .log .lvl-error { color: #dc2626; }
    @media (max-width: 820px) { .layout { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <main>
    <header>
      <h1 data-i18n="title">Codex Auto Reset</h1>
      <div style="display:flex; gap:10px; align-items:center;">
        <select id="locale" autocomplete="off">
          <option value="en">EN</option>
          <option value="zh">ZH</option>
        </select>
        <button id="refresh" type="button" data-i18n="refresh">Refresh</button>
      </div>
    </header>
    <div class="layout">
      <section class="panel">
        <h2 data-i18n="connection">Connection</h2>
        <div class="fields">
          <label><span data-i18n="managementKey">CPA management key</span>
            <input id="managementKey" type="password" autocomplete="off" spellcheck="false">
          </label>
          <div class="actions">
            <button id="load" type="button" data-i18n="load">Load status</button>
          </div>
          <label><span data-i18n="refreshInterval">Refresh interval</span>
            <input id="refreshInterval" placeholder="12h" spellcheck="false">
          </label>
          <label><span data-i18n="triggerLeadTime">Trigger lead time</span>
            <input id="triggerLeadTime" placeholder="6h" spellcheck="false">
          </label>
          <fieldset style="border:1px solid color-mix(in srgb, CanvasText 18%, Canvas 82%); border-radius:6px; padding:10px;">
            <legend style="font-size:13px; font-weight:600; padding:0 6px;" data-i18n="enabledAccounts">Enabled accounts</legend>
            <div id="accountCheckboxes" style="display:grid; gap:6px; max-height:200px; overflow-y:auto;">
              <span class="muted" data-i18n="loadAccountsPrompt">Click "Load status" to list accounts.</span>
            </div>
          </fieldset>
          <div class="actions">
            <button id="saveSettings" type="button" data-i18n="save">Save settings</button>
          </div>
        </div>
      </section>
      <section>
        <div id="accounts"><p class="muted" data-i18n="loadPrompt">Enter the CPA management key and click Load status.</p></div>
        <div class="panel" style="margin-top:16px;">
          <h2 data-i18n="logs">Logs</h2>
          <div id="logs" class="log"><span class="muted">-</span></div>
        </div>
      </section>
    </div>
  </main>
  <script>
    const I18N = {
      en: {
        title: "Codex Auto Reset",
        connection: "Connection",
        managementKey: "CPA management key",
        load: "Load status",
        refresh: "Refresh",
        refreshInterval: "Refresh interval",
        triggerLeadTime: "Trigger lead time",
        enabledAccounts: "Enabled accounts",
        save: "Save settings",
        loadPrompt: "Enter the CPA management key and click Load status.",
        loadAccountsPrompt: "Click \"Load status\" to list accounts.",
        noAccountsFound: "No Codex accounts found in CPA.",
        logs: "Logs",
        check: "Check",
        checkAll: "Check all",
        reset: "Reset now",
        creditsAvail: "Credits available",
        weeklyRemain: "Weekly remaining",
        nextExpiry: "Next credit expiry",
        nextState: "Next",
        lastCycle: "Last cycle",
        noAccounts: "No accounts enabled.",
        neverExpires: "never expires",
        expired: "expired",
        settingsSaved: "Settings saved.",
        confirmReset: "Force a reset cycle for this account now?",
        keyRequired: "Management key is required."
      },
      zh: {
        title: "Codex \u81ea\u52a8\u91cd\u7f6e",
        connection: "\u8fde\u63a5",
        managementKey: "CPA \u7ba1\u7406\u5bc6\u94a5",
        load: "\u52a0\u8f7d\u72b6\u6001",
        refresh: "\u5237\u65b0",
        refreshInterval: "\u5237\u65b0\u95f4\u9694",
        triggerLeadTime: "\u63d0\u524d\u89e6\u53d1\u65f6\u95f4",
        enabledAccounts: "\u542f\u7528\u7684\u8d26\u53f7",
        loadAccountsPrompt: "\u70b9\u51fb\u201c\u52a0\u8f7d\u72b6\u6001\u201d\u4ee5\u5217\u51fa\u8d26\u53f7\u3002",
        noAccountsFound: "\u672a\u5728 CPA \u4e2d\u627e\u5230 Codex \u8d26\u53f7\u3002",
        save: "\u4fdd\u5b58\u8bbe\u7f6e",
        loadPrompt: "\u8bf7\u8f93\u5165 CPA \u7ba1\u7406\u5bc6\u94a5\u5e76\u70b9\u51fb\u52a0\u8f7d\u72b6\u6001\u3002",
        logs: "\u65e5\u5fd7",
        check: "\u68c0\u67e5",
        checkAll: "\u68c0\u67e5\u5168\u90e8",
        reset: "\u7acb\u5373\u91cd\u7f6e",
        creditsAvail: "\u53ef\u7528\u91cd\u7f6e\u6b21\u6570",
        weeklyRemain: "\u5468\u989d\u5ea6\u5269\u4f59",
        nextExpiry: "\u4e0b\u4e00\u4e2a\u4fe1\u7528\u989d\u5ea6\u8fc7\u671f",
        nextState: "\u4e0b\u4e00\u8f6e",
        lastCycle: "\u4e0a\u4e00\u8f6e",
        noAccounts: "\u672a\u542f\u7528\u4efb\u4f55\u8d26\u53f7\u3002",
        neverExpires: "\u6c38\u4e0d\u8fc7\u671f",
        expired: "\u5df2\u8fc7\u671f",
        settingsSaved: "\u8bbe\u7f6e\u5df2\u4fdd\u5b58\u3002",
        confirmReset: "\u7acb\u5373\u5bf9\u8be5\u8d26\u53f7\u89e6\u53d1\u4e00\u6b21\u91cd\u7f6e\u6d41\u7a0b\uff1f",
        keyRequired: "\u9700\u8981\u7ba1\u7406\u5bc6\u94a5\u3002"
      }
    };
    function t(k) {
      const loc = document.getElementById('locale').value;
      return (I18N[loc] && I18N[loc][k]) || I18N.en[k] || k;
    }
    function applyI18n() {
      for (const el of document.querySelectorAll('[data-i18n]')) {
        el.textContent = t(el.dataset.i18n);
      }
    }
    function authHeaders() {
      const key = document.getElementById('managementKey').value.trim();
      if (!key) throw new Error(t('keyRequired'));
      const auth = key.toLowerCase().startsWith('bearer ') ? key : 'Bearer ' + key;
      // HTTP header names are case-insensitive; fetch() normalizes the key.
      return { 'authorization': auth };
    }
    async function apiGet(path) {
      const r = await fetch(path, { headers: authHeaders() });
      if (!r.ok) throw new Error(path + ': ' + r.status);
      return r.json();
    }
    async function apiSend(method, path, body) {
      const r = await fetch(path, {
        method, headers: { ...authHeaders(), 'Content-Type': 'application/json' },
        body: body ? JSON.stringify(body) : undefined
      });
      if (!r.ok) throw new Error(path + ': ' + r.status);
      return r.json().catch(() => ({}));
    }
    function fmtTime(ts) {
      if (!ts || String(ts).startsWith('0001-')) return '-';
      const d = new Date(ts);
      if (isNaN(d)) return ts;
      return d.toLocaleString();
    }
    function countdown(nextAt) {
      if (!nextAt || String(nextAt).startsWith('0001-')) return '';
      const d = new Date(nextAt);
      if (isNaN(d)) return '';
      const ms = d - Date.now();
      if (ms <= 0) return '';
      const s = Math.floor(ms / 1000);
      const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
      return (h ? h + 'h ' : '') + (m ? m + 'm ' : '') + sec + 's';
    }
    function escapeHTML(s) {
      return String(s == null ? '' : s)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
    function renderAccounts(status, logEntries) {
      const box = document.getElementById('accounts');
      const accts = (status && status.accounts) || {};
      const ids = Object.keys(accts);
      if (ids.length === 0) {
        box.innerHTML = '<p class="muted">' + escapeHTML(t('noAccounts')) + '</p>';
        return;
      }
      // Build per-account last-log lookup: most recent entry per scope.
      const lastLogByScope = {};
      (logEntries || []).forEach(function (e) {
        if (!e.scope) return;
        lastLogByScope[e.scope] = e;
      });
      box.innerHTML = ids.sort().map(function (id) {
        const a = accts[id] || {};
        const state = a.state || 'IDLE';
        const snap = a.last_snapshot || (a.attempt && a.attempt.pre_snapshot) || {};
        const credits = snap.available_count == null ? '-' : snap.available_count;
        const weekly = snap.weekly_pct == null || snap.weekly_pct < 0 ? '-' : snap.weekly_pct + '%';
        const nextAt = a.next_wake || '';
        const displayName = escapeHTML(accountNameMap[id] || id);

        // Credit list: ID + YYYY-MM-DD HH:MM:SS + remaining + target marker.
        let creditListHtml = '';
        if (snap.credits && snap.credits.length > 0) {
          const sorted = snap.credits.slice().sort(function (x, y) {
            const xe = x.expires_at || '9999', ye = y.expires_at || '9999';
            return xe.localeCompare(ye);
          });
          creditListHtml = sorted.map(function (c, idx) {
            const cid = escapeHTML(shortCreditID(c.id));
            const expiryStr = c.expires_at ? fmtExpiryCompact(c.expires_at) : '—';
            const remain = c.expires_at ? fmtRemainLocalized(c.expires_at) : t('neverExpires');
            const isTarget = idx === 0 ? ' <span class="muted" style="font-size:10px;">← target</span>' : '';
            return '<div style="font-size:12px; padding-left:12px;">• ' + cid + ' <span class="muted">' + escapeHTML(expiryStr) + '</span> ' + escapeHTML(remain) + isTarget + '</div>';
          }).join('');
        } else {
          creditListHtml = '<div class="muted" style="font-size:12px; padding-left:12px;">—</div>';
        }

        // Last log line for this account.
        const lastLog = lastLogByScope[id];
        const lastLogHtml = lastLog
          ? '<span class="k">' + escapeHTML(t('lastCycle')) + '</span><span style="font-size:11px;">' + escapeHTML(translateLog(lastLog.message)) + '</span>'
          : '';

        return '<div class="card">' +
          '<div class="card-head"><span class="auth-id">' + displayName + '</span>' +
          '<span class="state-badge ' + escapeHTML(state) + '">' + escapeHTML(state) + '</span></div>' +
          '<div class="kv">' +
          '<span class="k">' + escapeHTML(t('weeklyRemain')) + '</span><span>' + escapeHTML(weekly) + '</span>' +
          '<span class="k">' + escapeHTML(t('creditsAvail')) + '</span><span>' + escapeHTML(String(credits)) + '</span>' +
          '</div>' +
          creditListHtml +
          (lastLogHtml ? '<div class="kv" style="margin-top:6px;">' + lastLogHtml + '</div>' : '') +
          '<div class="next" style="margin-top:6px; font-size:12px; padding:6px 8px; border-radius:4px; background:color-mix(in srgb,#2563eb 6%,Canvas 94%);" data-next="' + escapeHTML(nextAt) + '">' +
          '<span class="k">' + escapeHTML(t('nextState')) + '</span> ' + escapeHTML(fmtTime(nextAt)) +
          ' <span class="countdown-text muted"></span></div>' +
          '<div class="actions" style="margin-top:8px;">' +
          '<button class="check-btn" data-auth="' + escapeHTML(id) + '">' + escapeHTML(t('check')) + '</button>' +
          '<button class="secondary reset-btn" data-auth="' + escapeHTML(id) + '">' + escapeHTML(t('reset')) + '</button>' +
          '</div>' +
          '</div>';
      }).join('');
    }

    // fmtRemain renders a human-readable remaining time from an ISO timestamp.
    function fmtRemain(iso) {
      if (!iso || iso.startsWith('0001-')) return t('neverExpires');
      const d = new Date(iso);
      if (isNaN(d)) return iso;
      const ms = d - Date.now();
      if (ms <= 0) return t('expired');
      const days = Math.floor(ms / 86400000);
      const hours = Math.floor((ms % 86400000) / 3600000);
      const mins = Math.floor((ms % 3600000) / 60000);
      if (days > 0) return days + 'd' + (hours > 0 ? ' ' + hours + 'h' : '');
      if (hours > 0) return hours + 'h' + (mins > 0 ? ' ' + mins + 'm' : '');
      return mins + 'm';
    }

    // fmtExpiryCompact renders an ISO timestamp as YYYY-MM-DD HH:MM:SS
    // (full readable date for credit expiry display).
    function fmtExpiryCompact(iso) {
      if (!iso || iso.startsWith('0001-')) return '—';
      const d = new Date(iso);
      if (isNaN(d)) return iso;
      const yyyy = d.getFullYear();
      const MM = String(d.getMonth() + 1).padStart(2, '0');
      const dd = String(d.getDate()).padStart(2, '0');
      const HH = String(d.getHours()).padStart(2, '0');
      const mm = String(d.getMinutes()).padStart(2, '0');
      const ss = String(d.getSeconds()).padStart(2, '0');
      return yyyy + '-' + MM + '-' + dd + ' ' + HH + ':' + mm + ':' + ss;
    }

    // shortCreditID extracts the hex prefix from a RateLimitResetCredit ID.
    // "RateLimitResetCredit_d7087f83469c819182a87d5916512c9c" -> "d7087f83…"
    function shortCreditID(id) {
      if (!id) return '';
      const prefix = 'RateLimitResetCredit_';
      var s = id.startsWith(prefix) ? id.substring(prefix.length) : id;
      return s.length > 10 ? s.substring(0, 8) + '…' : s;
    }

    // fmtRemainLocalized renders remaining time in the selected language:
    // ZH: "8天9小时", EN: "8d 9h".
    function fmtRemainLocalized(iso) {
      if (!iso || iso.startsWith('0001-')) return t('neverExpires');
      const d = new Date(iso);
      if (isNaN(d)) return iso;
      const ms = d - Date.now();
      if (ms <= 0) return t('expired');
      const days = Math.floor(ms / 86400000);
      const hours = Math.floor((ms % 86400000) / 3600000);
      const mins = Math.floor((ms % 3600000) / 60000);
      const zh = document.getElementById('locale').value === 'zh';
      const dayUnit = zh ? '天' : 'd';
      const hourUnit = zh ? '小时' : 'h';
      const minUnit = zh ? '分' : 'm';
      const parts = [];
      if (days > 0) parts.push(days + dayUnit);
      if (hours > 0) parts.push(hours + hourUnit);
      if (days === 0 && hours === 0 && mins > 0) parts.push(mins + minUnit);
      return parts.join(zh ? '' : ' ') || (zh ? '不到1分' : '<1m');
    }
    // translateLog converts an English FSM log message to the selected UI
    // language. Uses regex replacements for the common patterns so the
    // dynamic parts (credit IDs, durations, counts) are preserved.
    function translateLog(msg) {
      if (document.getElementById('locale').value !== 'zh') return msg;
      // State names
      const stateMap = {
        'IDLE': 'IDLE', 'ARMED': 'ARMED', 'CONFIRMING': 'CONFIRMING',
        'RESETTING': 'RESETTING', 'VERIFYING': 'VERIFYING', 'DONE': 'DONE'
      };
      let z = msg;
      const rules = [
        [/no available credits/g, '未发现可用重置次数'],
        [/credit ([a-f0-9…]*) expires in ([^;]+); not near trigger window \(will arm ([^)]+) before expiry[^)]*\)/g,
         '重置次数 $1 将在 $2 后过期；尚未进入触发窗口（将在过期前 $3 进入 ARMED）'],
        [/credit ([a-f0-9…]*) expires in ([^;]+); not near trigger window.*/g,
         '重置次数 $1 将在 $2 后过期；尚未进入触发窗口'],
        [/credit ([a-f0-9…]*) armed; will reset at ([^ ]+) \(expiry in ([^)]+)\)/g,
         '重置次数 $1 已进入 ARMED；将在 $2 触发重置（过期前还有 $3）'],
        [/trigger time reached, confirming before reset/g, '触发时间已到，正在确认后重置'],
        [/confirmed target credit ([a-f0-9…]*) \(weekly was (\d+%%)\); sending reset request/g,
         '已确认目标重置次数 $1（周额度 $2）；正在发送重置请求'],
        [/reset request accepted \(code=([^,]+), windows_reset=(\d+)\); will verify in ([^)]+)/g,
         '重置请求已接受（code=$1，重置窗口=$2）；$3 后验证'],
        [/reset stopped: server returned (.+)/g, '重置已停止：服务端返回 $1'],
        [/reset stopped: (.+)/g, '重置已停止：$1'],
        [/reset failed \(attempt (\d+)\): (.+); retrying with same idempotency key/g,
         '重置失败（第 $1 次）：$2；使用相同幂等键重试'],
        [/retries exhausted after (\d+) attempts; last error: (.+)/g,
         '重试 $1 次后放弃；最后错误：$2'],
        [/reset verified: credits (\d+)→(\d+), weekly (\d+%%)→(\d+%%)/g,
         '重置验证通过：重置次数 $1→$2，周额度 $3→$4'],
        [/reset partial: credits ok but weekly (\d+%%)→(\d+%%) \(delayed\?\)/g,
         '重置部分完成：次数已扣但周额度 $1→$2（延迟？）'],
        [/verification mismatch: target gone=(\w+), count-1=(\w+), quota up=(\w+) — halted/g,
         '验证不匹配：目标已扣=$1，次数-1=$2，额度回升=$3 —— 已停止'],
        [/verification failed: (.+)/g, '验证失败：$1'],
        [/target credit vanished before reset/g, '目标重置次数在重置前已消失'],
        [/armed wake at T, confirming/g, '触发时间已到，正在确认'],
      ];
      for (const r of rules) {
        z = z.replace(r[0], r[1]);
      }
      return z;
    }

    function renderLogs(entries) {
      const box = document.getElementById('logs');
      const list = entries || [];
      if (list.length === 0) { box.innerHTML = '<span class="muted">-</span>'; return; }
      box.innerHTML = list.slice(-100).reverse().map(function (e) {
        const lvl = (e.level || 'info').toUpperCase();
        const scope = accountNameMap[e.scope] || e.scope || '';
        const state = e.state || '';
        const msg = translateLog(e.message || '');
        return '<div class="line"><span class="ts">[' + escapeHTML(fmtTime(e.timestamp)) + ']</span> ' +
          '<span class="lvl-' + escapeHTML(e.level || 'info') + '">' + escapeHTML(lvl) + '</span> ' +
          '<span class="muted">[' + escapeHTML(scope) + '/' + escapeHTML(state) + ']</span> ' +
          escapeHTML(msg) + '</div>';
      }).join('');
    }
    // accountNameMap caches auth_index -> human-readable name from /accounts,
    // so the right-panel cards can show "codex-44411af1-…-team.json" instead
    // of the raw hex auth_index.
    var accountNameMap = {};

    async function loadStatus() {
      try {
        const results = await Promise.all([
          apiGet('/v0/management/plugins/codex-auto-reset/status'),
          apiGet('/v0/management/plugins/codex-auto-reset/logs'),
          apiGet('/v0/management/plugins/codex-auto-reset/accounts').catch(function () { return { accounts: [] }; }),
        ]);
        const status = results[0], logsResp = results[1], acctsResp = results[2];
        // Build the name cache from /accounts.
        accountNameMap = {};
        (acctsResp.accounts || []).forEach(function (a) {
          accountNameMap[a.auth_index] = a.name || a.email || a.auth_index;
        });
        renderAccounts(status, logsResp.entries || []);
        renderLogs(logsResp.entries || []);
        const c = status.config || {};
        document.getElementById('refreshInterval').value = (c.refresh_interval || '').toString();
        document.getElementById('triggerLeadTime').value = (c.trigger_lead_time || '').toString();
        renderAccountCheckboxes(acctsResp.accounts || []);
      } catch (e) { alert(e.message); }
    }
    function renderAccountCheckboxes(accts) {
      var box = document.getElementById('accountCheckboxes');
      if (!accts || accts.length === 0) {
        box.innerHTML = '<label style="font-size:12px;" class="muted">' + escapeHTML(t('noAccountsFound')) + '</label>';
        return;
      }
      box.innerHTML = accts.map(function (a) {
        var checked = a.enabled ? ' checked' : '';
        // a.name is already the most informative label (ID/file name preferred).
        var label = escapeHTML(a.name || a.email || a.auth_index);
        return '<label style="display:flex; align-items:center; gap:8px; font-size:13px; font-weight:500;">' +
               '<input type="checkbox" class="acct-checkbox" value="' + escapeHTML(a.auth_index) + '"' + checked + ' style="width:auto; margin:0;">' +
               '<span>' + label + '</span></label>';
      }).join('');
    }
    function getEnabledAccounts() {
      return Array.prototype.slice.call(document.querySelectorAll('.acct-checkbox:checked'))
        .map(function (cb) { return cb.value; });
    }
    async function saveSettings() {
      try {
        await apiSend('PUT', '/v0/management/plugins/codex-auto-reset/settings', {
          refresh_interval: document.getElementById('refreshInterval').value.trim(),
          trigger_lead_time: document.getElementById('triggerLeadTime').value.trim(),
          enabled_accounts: getEnabledAccounts()
        });
        alert(t('settingsSaved'));
      } catch (e) { alert(e.message); }
    }
    async function checkOne(id) {
      try { await apiSend('POST', '/v0/management/plugins/codex-auto-reset/check', { auth_index: id }); await loadStatus(); }
      catch (e) { alert(e.message); }
    }
    async function resetOne(id) {
      if (!confirm(t('confirmReset'))) return;
      try { await apiSend('POST', '/v0/management/plugins/codex-auto-reset/reset', { auth_index: id }); await loadStatus(); }
      catch (e) { alert(e.message); }
    }
    function onLoad() {
      document.getElementById('locale').value = (navigator.language || '').toLowerCase().indexOf('zh') === 0 ? 'zh' : 'en';
      document.getElementById('locale').addEventListener('change', applyI18n);
      document.getElementById('refresh').addEventListener('click', loadStatus);
      document.getElementById('load').addEventListener('click', loadStatus);
      document.getElementById('saveSettings').addEventListener('click', saveSettings);
      document.addEventListener('click', function (ev) {
        const target = ev.target;
        if (!(target instanceof Element)) return;
        if (target.classList.contains('check-btn')) { checkOne(target.dataset.auth || ''); return; }
        if (target.classList.contains('reset-btn')) { resetOne(target.dataset.auth || ''); return; }
      });
      applyI18n();
      setInterval(function () {
        for (const el of document.querySelectorAll('.next[data-next]')) {
          const c = countdown(el.dataset.next || '');
          const span = el.querySelector('.countdown-text');
          if (span) span.textContent = c ? '(' + c + ')' : '';
        }
      }, 1000);
    }
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', onLoad);
    } else {
      onLoad();
    }
  </script>
</body>
</html>`
}
