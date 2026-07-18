package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const pluginID = "codex-auto-reset"

// managementHandlers owns the plugin's mutable state and serves the JSON
// routes from spec §9. The HTML resource page (Task 12) is rendered by
// renderStatusHTML, called from the resource/status route.
type managementHandlers struct {
	mu        sync.Mutex
	state     *PluginState
	statePath string
	worker    *Worker
}

func newManagementHandlers(state *PluginState, statePath string, worker *Worker) *managementHandlers {
	return &managementHandlers{state: state, statePath: statePath, worker: worker}
}

// handle dispatches one management request. path may carry either the full
// /v0/management/plugins/codex-auto-reset prefix, the bare codex-auto-reset
// segment, or already be stripped (tests pass various forms). We locate the
// plugin ID segment and route on whatever follows it. Returns (HTTP status,
// response body bytes).
func (h *managementHandlers) handle(method, path string, headers http.Header, body []byte) (int, []byte) {
	if idx := strings.LastIndex(path, pluginID); idx >= 0 {
		path = path[idx+len(pluginID):]
	}
	path = strings.Trim(path, "/")
	switch {
	case method == http.MethodGet && path == "status":
		return h.statusJSON()
	case method == http.MethodGet && path == "logs":
		return h.logsJSON()
	case method == http.MethodPut && path == "settings":
		return h.putSettings(body)
	case method == http.MethodPost && path == "check":
		return h.checkOne(body)
	case method == http.MethodPost && path == "check/all":
		if h.worker != nil {
			h.worker.TriggerCheck("")
		}
		return jsonOK(map[string]any{"triggered": "all"})
	case method == http.MethodPost && path == "reset":
		return h.resetOne(body)
	case method == http.MethodGet && path == "export":
		return h.exportState()
	case method == http.MethodPost && path == "import":
		return h.importState(body)
	case method == http.MethodGet && (path == "resource/status" || path == "codex-auto-reset/resource/status"):
		return http.StatusOK, []byte(renderStatusHTML(h.state))
	}
	return jsonStatus(http.StatusNotFound, map[string]any{"error": "route not found"})
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
	return jsonOK(map[string]any{
		"refresh_interval": formatDuration(cfg.RefreshInterval),
		"trigger_lead_time": formatDuration(cfg.TriggerLeadTime),
		"enabled_accounts":  cfg.EnabledAccounts,
	})
}

// formatDuration renders a time.Duration as the shortest canonical Go duration
// string (e.g. 4h, 2h, 30m0s) — i.e. what the user typed. time.Duration.String()
// always emits Hh0m0s for whole-hour durations, but the management API echoes
// the minimal form so round-trips through the UI stay clean.
func formatDuration(d time.Duration) string {
	s := d.String()
	// time.Duration.String() never produces trailing zeros for whole
	// values except the canonical "0s"; trim a trailing "0m0s" to get "4h"
	// from "4h0m0s". This mirrors what time.ParseDuration accepts.
	const suffix = "0m0s"
	if strings.HasSuffix(s, suffix) {
		return strings.TrimSuffix(s, suffix)
	}
	return s
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
          <label><span data-i18n="enabledAccounts">Enabled accounts (one per line)</span>
            <textarea id="enabledAccounts" rows="4" spellcheck="false"></textarea>
          </label>
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
        enabledAccounts: "Enabled accounts (one per line)",
        save: "Save settings",
        loadPrompt: "Enter the CPA management key and click Load status.",
        logs: "Logs",
        check: "Check",
        checkAll: "Check all",
        reset: "Reset now",
        creditsAvail: "Credits available",
        nextState: "Next",
        lastCycle: "Last cycle",
        noAccounts: "No accounts enabled.",
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
        enabledAccounts: "\u542f\u7528\u7684\u8d26\u53f7\uff08\u6bcf\u884c\u4e00\u4e2a\uff09",
        save: "\u4fdd\u5b58\u8bbe\u7f6e",
        loadPrompt: "\u8bf7\u8f93\u5165 CPA \u7ba1\u7406\u5bc6\u94a5\u5e76\u70b9\u51fb\u52a0\u8f7d\u72b6\u6001\u3002",
        logs: "\u65e5\u5fd7",
        check: "\u68c0\u67e5",
        checkAll: "\u68c0\u67e5\u5168\u90e8",
        reset: "\u7acb\u5373\u91cd\u7f6e",
        creditsAvail: "\u53ef\u7528\u91cd\u7f6e\u6b21\u6570",
        nextState: "\u4e0b\u4e00\u8f6e",
        lastCycle: "\u4e0a\u4e00\u8f6e",
        noAccounts: "\u672a\u542f\u7528\u4efb\u4f55\u8d26\u53f7\u3002",
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
    function renderAccounts(status) {
      const box = document.getElementById('accounts');
      const accts = (status && status.accounts) || {};
      const ids = Object.keys(accts);
      if (ids.length === 0) {
        box.innerHTML = '<p class="muted">' + escapeHTML(t('noAccounts')) + '</p>';
        return;
      }
      box.innerHTML = ids.sort().map(function (id) {
        const a = accts[id] || {};
        const state = a.state || 'IDLE';
        const snap = (a.attempt && a.attempt.pre_snapshot) || {};
        const credits = snap.available_count == null ? '-' : snap.available_count;
        const nextAt = a.next_check_at || '';
        return '<div class="card">' +
          '<div class="card-head"><span class="auth-id">' + escapeHTML(id) + '</span>' +
          '<span class="state-badge ' + escapeHTML(state) + '">' + escapeHTML(state) + '</span></div>' +
          '<div class="kv">' +
          '<span class="k">' + escapeHTML(t('creditsAvail')) + '</span><span>' + escapeHTML(credits) + '</span>' +
          '<span class="k">' + escapeHTML(t('nextState')) + '</span><span class="next" data-next="' + escapeHTML(nextAt) + '">' + escapeHTML(fmtTime(nextAt)) + ' <span class="countdown"></span></span>' +
          '</div>' +
          '<div class="actions" style="margin-top:8px;">' +
          '<button class="check-btn" data-auth="' + escapeHTML(id) + '">' + escapeHTML(t('check')) + '</button>' +
          '<button class="secondary reset-btn" data-auth="' + escapeHTML(id) + '">' + escapeHTML(t('reset')) + '</button>' +
          '</div>' +
          '</div>';
      }).join('');
    }
    function renderLogs(entries) {
      const box = document.getElementById('logs');
      const list = entries || [];
      if (list.length === 0) { box.innerHTML = '<span class="muted">-</span>'; return; }
      box.innerHTML = list.slice(-100).reverse().map(function (e) {
        const lvl = (e.level || 'info').toUpperCase();
        return '<div class="line"><span class="ts">[' + escapeHTML(fmtTime(e.timestamp)) + ']</span> ' +
          '<span class="lvl-' + escapeHTML(e.level || 'info') + '">' + escapeHTML(lvl) + '</span> ' +
          '<span>[' + escapeHTML(e.scope || '') + '/' + escapeHTML(e.state || '') + ']</span> ' +
          escapeHTML(e.message) + '</div>';
      }).join('');
    }
    async function loadStatus() {
      try {
        const status = await apiGet('/v0/management/plugins/codex-auto-reset/status');
        const logsResp = await apiGet('/v0/management/plugins/codex-auto-reset/logs');
        renderAccounts(status);
        renderLogs(logsResp.entries || []);
        const c = status.config || {};
        document.getElementById('refreshInterval').value = (c.refresh_interval || '').toString();
        document.getElementById('triggerLeadTime').value = (c.trigger_lead_time || '').toString();
        document.getElementById('enabledAccounts').value = (c.enabled_accounts || []).join('\n');
      } catch (e) { alert(e.message); }
    }
    async function saveSettings() {
      try {
        const enabled = document.getElementById('enabledAccounts').value
          .split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
        await apiSend('PUT', '/v0/management/plugins/codex-auto-reset/settings', {
          refresh_interval: document.getElementById('refreshInterval').value.trim(),
          trigger_lead_time: document.getElementById('triggerLeadTime').value.trim(),
          enabled_accounts: enabled
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
        for (const el of document.querySelectorAll('[data-next]')) {
          const c = countdown(el.dataset.next || '');
          const span = el.querySelector('.countdown');
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
