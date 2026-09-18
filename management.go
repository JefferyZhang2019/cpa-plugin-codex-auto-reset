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
	case method == http.MethodGet && path == "/debug/auth-list":
		return h.debugAuthList()
	case method == http.MethodGet && path == "/debug/usage":
		return h.debugUsage()
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
	// Deep-copy reset history.
	history := make([]ResetRecord, len(h.state.ResetHistory))
	copy(history, h.state.ResetHistory)
	out := map[string]any{
		"plugin":        pluginID,
		"config":        h.state.Config,
		"accounts":      accts,
		"reset_history": history,
	}
	return jsonOK(out)
}

// accountsJSON returns the list of Codex accounts CPA knows about, annotated
// with whether each is currently enabled. The UI renders these as checkboxes.
// debugUsage fetches the raw /wham/usage response for the first enabled
// account and returns it verbatim. Temporary diagnostic to determine the
// real wire shape of weekly quota data.
func (h *managementHandlers) debugUsage() (int, []byte, string) {
	h.mu.Lock()
	enabled := h.state.Config.EnabledAccounts
	h.mu.Unlock()
	if len(enabled) == 0 {
		st, b := jsonStatus(http.StatusBadRequest, map[string]any{"error": "no enabled accounts"})
		return st, b, "application/json"
	}
	if h.worker == nil {
		st, b := jsonStatus(http.StatusServiceUnavailable, map[string]any{"error": "worker not running"})
		return st, b, "application/json"
	}
	// Grab the first FSM's credentials + client.
	var fsm *AccountFSM
	h.worker.EachFSM(func(f *AccountFSM) {
		if fsm == nil {
			fsm = f
		}
	})
	if fsm == nil {
		st, b := jsonStatus(http.StatusServiceUnavailable, map[string]any{"error": "no FSM available"})
		return st, b, "application/json"
	}
	// Do a raw HTTP GET to see the actual response body.
	cli := &OpenAIClient{}
	req, err := http.NewRequest(http.MethodGet, defaultBaseURL+usageEndpointPath, nil)
	if err != nil {
		st, b := jsonStatus(http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return st, b, "application/json"
	}
	req.Header.Set("Authorization", "Bearer "+fsm.Creds.AccessToken)
	if fsm.Creds.ChatGPTAccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", fsm.Creds.ChatGPTAccountID)
	}
	req.Header.Set("User-Agent", codexUserAgent)
	body, err := cli.do(req)
	if err != nil {
		st, b := jsonStatus(http.StatusBadGateway, map[string]any{"error": err.Error()})
		return st, b, "application/json"
	}
	return http.StatusOK, body, "application/json"
}

// debugAuthList dumps the raw host.auth.list result with per-entry filter
// diagnostics, so a remote deployment can see exactly why accounts are or
// aren't matching the codex filter.
func (h *managementHandlers) debugAuthList() (int, []byte, string) {
	auths, err := listAuthsViaHost()
	if err != nil {
		st, b := jsonStatus(http.StatusBadGateway, map[string]any{"error": "host.auth.list: " + err.Error()})
		return st, b, "application/json"
	}
	type diag struct {
		ID          string `json:"id"`
		AuthIndex   string `json:"auth_index"`
		Provider    string `json:"provider"`
		Type        string `json:"type"`
		Name        string `json:"name"`
		Email       string `json:"email"`
		Disabled    bool   `json:"disabled"`
		Unavailable bool   `json:"unavailable"`
		MatchesCodex bool  `json:"matches_codex"`
		Skipped     string `json:"skipped_reason,omitempty"`
	}
	out := make([]diag, 0, len(auths))
	for _, a := range auths {
		d := diag{
			ID:          a.ID,
			AuthIndex:   a.AuthIndex,
			Provider:    a.Provider,
			Type:        a.Type,
			Name:        a.Name,
			Email:       a.Email,
			Disabled:    a.Disabled,
			Unavailable: a.Unavailable,
		}
		if strings.EqualFold(a.Provider, "codex") || strings.EqualFold(a.Type, "codex") {
			d.MatchesCodex = true
			if a.Disabled || a.Unavailable {
				d.Skipped = "disabled/unavailable"
			}
		} else {
			d.Skipped = "provider/type not codex"
		}
		out = append(out, d)
	}
	st, b := jsonOK(map[string]any{"total": len(auths), "entries": out})
	return st, b, "application/json"
}

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
      --bg: #f6f8fa;
      --surface: #ffffff;
      --surface-2: #f6f8fa;
      --border: #e1e4e8;
      --border-strong: #d0d7de;
      --text: #24292e;
      --text-muted: #6a737d;
      --primary: #0969da;
      --primary-soft: rgba(9, 105, 218, 0.10);
      --primary-text: #ffffff;
      --success: #1a7f37;
      --success-soft: rgba(26, 127, 55, 0.12);
      --warning: #9a6700;
      --warning-soft: rgba(154, 103, 0, 0.14);
      --danger: #cf222e;
      --danger-soft: rgba(207, 34, 46, 0.10);
      --radius: 8px;
      --radius-sm: 6px;
      --radius-lg: 12px;
      --shadow: 0 1px 3px rgba(0,0,0,0.08);
      --shadow-md: 0 3px 8px rgba(0,0,0,0.08);
      --sidebar-width: 300px;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #0d1117;
        --surface: #161b22;
        --surface-2: #21262d;
        --border: #30363d;
        --border-strong: #444c56;
        --text: #e6edf3;
        --text-muted: #8b949e;
        --primary: #58a6ff;
        --primary-soft: rgba(88, 166, 255, 0.14);
        --primary-text: #0d1117;
        --success: #3fb950;
        --success-soft: rgba(63, 185, 80, 0.16);
        --warning: #d29922;
        --warning-soft: rgba(210, 153, 34, 0.16);
        --danger: #f85149;
        --danger-soft: rgba(248, 81, 73, 0.16);
        --shadow: 0 1px 3px rgba(0,0,0,0.3);
        --shadow-md: 0 3px 8px rgba(0,0,0,0.4);
      }
    }
    * { box-sizing: border-box; }
    html, body { margin: 0; padding: 0; }
    body {
      background: var(--bg);
      color: var(--text);
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
      font-size: 14px;
      line-height: 1.5;
      -webkit-font-smoothing: antialiased;
    }
    .app { display: flex; min-height: 100vh; flex-direction: column; }
    .topbar {
      display: flex; align-items: center; justify-content: space-between;
      gap: 16px; padding: 14px 24px;
      background: var(--surface); border-bottom: 1px solid var(--border);
      position: sticky; top: 0; z-index: 10;
    }
    .brand { display: flex; align-items: center; gap: 10px; }
    .brand .logo {
      width: 28px; height: 28px; border-radius: 6px;
      background: linear-gradient(135deg, var(--primary), #6cb6ff);
      color: #fff; font-weight: 800; display: grid; place-items: center;
      font-size: 14px;
    }
    h1 { margin: 0; font-size: 17px; font-weight: 700; }
    .topbar-actions { display: flex; align-items: center; gap: 8px; }
    .locale-switch {
      display: inline-flex; background: var(--surface-2);
      border: 1px solid var(--border); border-radius: var(--radius-sm);
      overflow: hidden;
    }
    .locale-switch button {
      border: 0; background: transparent; color: var(--text-muted);
      padding: 6px 12px; cursor: pointer; font-weight: 600; font-size: 12px;
      border-radius: 0;
    }
    .locale-switch button.active {
      background: var(--primary); color: var(--primary-text);
    }

    .layout {
      display: grid; grid-template-columns: var(--sidebar-width) minmax(0, 1fr);
      gap: 20px; padding: 20px 24px; max-width: 1400px; width: 100%;
      margin: 0 auto; flex: 1; align-items: start;
    }

    .sidebar { position: sticky; top: 72px; display: grid; gap: 16px; }

    .main { display: grid; gap: 16px; min-width: 0; }

    .card {
      background: var(--surface); border: 1px solid var(--border);
      border-radius: var(--radius-lg); box-shadow: var(--shadow);
      padding: 16px; margin-bottom: 0;
    }
    .panel {
      background: var(--surface); border: 1px solid var(--border);
      border-radius: var(--radius-lg); box-shadow: var(--shadow);
      padding: 16px;
    }
    h2 { margin: 0 0 12px; font-size: 13px; font-weight: 700;
      color: var(--text); text-transform: uppercase; letter-spacing: 0.04em;
      display: flex; align-items: center; gap: 6px;
    }
    h2 .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--primary); }

    label.field { display: grid; gap: 5px; font-size: 12px; font-weight: 600; color: var(--text); }
    input, select, textarea {
      width: 100%; border: 1px solid var(--border-strong); border-radius: var(--radius-sm);
      padding: 8px 10px; background: var(--surface); color: var(--text);
      font: inherit; transition: border-color 0.12s, box-shadow 0.12s;
    }
    input:focus, select:focus, textarea:focus {
      outline: none; border-color: var(--primary);
      box-shadow: 0 0 0 3px var(--primary-soft);
    }
    input[type="checkbox"] { width: auto; }
    button {
      font: inherit; font-weight: 600; border: 1px solid transparent;
      border-radius: var(--radius-sm); padding: 8px 14px; cursor: pointer;
      transition: background 0.12s, border-color 0.12s, opacity 0.12s;
    }
    button.primary { background: var(--primary); color: var(--primary-text); }
    button.primary:hover { filter: brightness(0.95); }
    button.secondary {
      background: var(--surface); border-color: var(--border-strong); color: var(--text);
    }
    button.secondary:hover { background: var(--surface-2); }
    button.danger { background: var(--danger); color: #fff; }
    button:disabled { opacity: .5; cursor: not-allowed; }
    .actions { display: flex; gap: 8px; flex-wrap: wrap; }
    .actions button { width: auto; }
    .field-stack { display: grid; gap: 12px; }

    /* Account checkboxes */
    .acct-list { display: grid; gap: 4px; max-height: 220px; overflow-y: auto;
      padding-right: 4px; margin: 0 -4px 0 0; }
    .acct-list::-webkit-scrollbar { width: 6px; }
    .acct-list::-webkit-scrollbar-thumb { background: var(--border-strong); border-radius: 3px; }
    .acct-item { display: flex; align-items: center; gap: 8px; font-size: 12px; font-weight: 500;
      padding: 5px 8px; border-radius: var(--radius-sm); cursor: pointer; word-break: break-all; }
    .acct-item:hover { background: var(--surface-2); }
    .acct-item input { margin: 0; }

    /* Stats panel */
    .stats-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; }
    .stat { background: var(--surface-2); border: 1px solid var(--border);
      border-radius: var(--radius); padding: 12px 14px; }
    .stat .stat-label { font-size: 11px; font-weight: 600; color: var(--text-muted);
      text-transform: uppercase; letter-spacing: 0.04em; }
    .stat .stat-value { font-size: 22px; font-weight: 700; margin-top: 4px; line-height: 1.1; }
    .stat.success .stat-value { color: var(--success); }
    .stat.danger .stat-value { color: var(--danger); }
    .stat.primary .stat-value { color: var(--primary); }
    .stat .stat-sub { font-size: 11px; color: var(--text-muted); margin-top: 2px; }

    /* Account cards */
    .acct-card {
      background: var(--surface); border: 1px solid var(--border);
      border-radius: var(--radius-lg); box-shadow: var(--shadow);
      padding: 16px; margin-bottom: 12px;
      transition: box-shadow 0.15s, border-color 0.15s;
    }
    .acct-card:hover { box-shadow: var(--shadow-md); border-color: var(--border-strong); }
    .card-head { display: flex; justify-content: space-between; align-items: center; gap: 10px;
      margin-bottom: 12px; flex-wrap: wrap; }
    .auth-id { font-weight: 700; font-size: 14px; word-break: break-all; }
    .state-badge {
      font-size: 10px; font-weight: 800; letter-spacing: 0.05em;
      padding: 3px 10px; border-radius: 999px; text-transform: uppercase;
      background: var(--primary-soft); color: var(--primary); white-space: nowrap;
      border: 1px solid transparent;
    }
    .state-badge.ARMED, .state-badge.CONFIRMING, .state-badge.RESETTING, .state-badge.VERIFYING {
      background: var(--warning-soft); color: var(--warning);
    }
    .state-badge.DONE { background: var(--success-soft); color: var(--success); }

    /* Progress bar */
    .progress { margin: 12px 0; }
    .progress-head { display: flex; justify-content: space-between; align-items: baseline;
      font-size: 12px; margin-bottom: 5px; }
    .progress-head .k { color: var(--text-muted); font-weight: 600; }
    .progress-head .v { font-weight: 700; }
    .progress-track {
      height: 8px; background: var(--surface-2); border-radius: 999px;
      overflow: hidden; border: 1px solid var(--border);
    }
    .progress-fill {
      height: 100%; border-radius: 999px;
      background: linear-gradient(90deg, var(--success), #46c75a);
      transition: width 0.4s ease;
    }
    .progress-fill.warn { background: linear-gradient(90deg, var(--warning), #e8b339); }
    .progress-fill.danger { background: linear-gradient(90deg, var(--danger), #ff6b6b); }
    .progress-fill.unknown { background: var(--border-strong); }

    .card-row { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 10px;
      margin-top: 12px; }
    .mini-stat { background: var(--surface-2); border: 1px solid var(--border);
      border-radius: var(--radius-sm); padding: 8px 10px; }
    .mini-stat .k { font-size: 10px; font-weight: 600; color: var(--text-muted);
      text-transform: uppercase; letter-spacing: 0.04em; }
    .mini-stat .v { font-size: 15px; font-weight: 700; margin-top: 2px; }

    .kv { display: grid; grid-template-columns: auto 1fr; gap: 4px 12px; font-size: 12px; }
    .kv .k { color: var(--text-muted); font-weight: 500; }
    .muted { color: var(--text-muted); font-size: 12px; }

    .credit-list { margin-top: 8px; display: grid; gap: 3px; }
    .credit-line { font-size: 12px; padding-left: 4px; word-break: break-all; }
    .credit-line .cid { font-family: ui-monospace, "SFMono-Regular", Consolas, monospace; font-weight: 600; }

    .history { margin-top: 10px; padding-top: 10px; border-top: 1px solid var(--border); }
    .history .k { font-size: 11px; font-weight: 600; color: var(--text-muted);
      text-transform: uppercase; letter-spacing: 0.04em; }
    .history-line { font-size: 11px; padding-left: 4px; margin-top: 3px; }

    .next {
      margin-top: 10px; font-size: 12px; padding: 10px 12px; border-radius: var(--radius);
      background: var(--primary-soft); border: 1px solid transparent;
    }
    .next .row { display: flex; gap: 6px; align-items: baseline; }
    .next .row + .row { margin-top: 4px; }
    .next .k { color: var(--text-muted); font-weight: 600; }
    .countdown { color: var(--text-muted); font-weight: 600; }

    .card-actions { display: flex; gap: 8px; margin-top: 12px; flex-wrap: wrap; }
    .card-actions button { width: auto; padding: 6px 14px; font-size: 13px; }

    /* Logs */
    .log {
      font-family: ui-monospace, "SFMono-Regular", "Cascadia Code", Consolas, monospace;
      font-size: 11.5px; line-height: 1.55; max-height: 340px; overflow-y: auto;
      padding: 12px; border-radius: var(--radius); background: var(--surface-2);
      border: 1px solid var(--border);
    }
    .log::-webkit-scrollbar { width: 8px; }
    .log::-webkit-scrollbar-thumb { background: var(--border-strong); border-radius: 4px; }
    .log .line { white-space: pre-wrap; word-break: break-word; padding: 1px 0; }
    .log .ts { color: var(--text-muted); }
    .log .lvl-warn { color: var(--warning); font-weight: 700; }
    .log .lvl-error { color: var(--danger); font-weight: 700; }
    .log .lvl-info { color: var(--primary); font-weight: 700; }

    .empty {
      text-align: center; padding: 32px 16px; color: var(--text-muted);
      font-size: 13px;
    }

    /* Mobile responsive */
    @media (max-width: 860px) {
      .layout { grid-template-columns: 1fr; padding: 16px; }
      .sidebar { position: static; }
      .stats-grid { grid-template-columns: repeat(2, 1fr); }
      .topbar { padding: 12px 16px; }
    }
    @media (max-width: 480px) {
      .stats-grid { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <div class="app">
    <header class="topbar">
      <div class="brand">
        <div class="logo">CR</div>
        <h1 data-i18n="title">Codex Auto Reset</h1>
      </div>
      <div class="topbar-actions">
        <div class="locale-switch" id="localeSwitch">
          <button type="button" data-locale="en">EN</button>
          <button type="button" data-locale="zh">ZH</button>
        </div>
        <select id="locale" autocomplete="off" style="display:none;">
          <option value="en">EN</option>
          <option value="zh">ZH</option>
        </select>
        <button id="refresh" type="button" class="secondary" data-i18n="refresh">Refresh</button>
      </div>
    </header>
    <div class="layout">
      <aside class="sidebar">
        <section class="panel">
          <h2><span class="dot"></span><span data-i18n="connection">Connection</span></h2>
          <div class="field-stack">
            <label class="field"><span data-i18n="managementKey">CPA management key</span>
              <input id="managementKey" type="password" autocomplete="off" spellcheck="false">
            </label>
            <div class="actions">
              <button id="load" type="button" class="primary" data-i18n="load">Load status</button>
            </div>
          </div>
        </section>
        <section class="panel">
          <h2><span class="dot"></span><span data-i18n="settings">Settings</span></h2>
          <div class="field-stack">
            <label class="field"><span data-i18n="refreshInterval">Refresh interval</span>
              <input id="refreshInterval" placeholder="12h" spellcheck="false">
            </label>
            <label class="field"><span data-i18n="triggerLeadTime">Trigger lead time</span>
              <input id="triggerLeadTime" placeholder="6h" spellcheck="false">
            </label>
            <div>
              <div style="font-size:12px; font-weight:600; margin-bottom:6px;" data-i18n="enabledAccounts">Enabled accounts</div>
              <div id="accountCheckboxes" class="acct-list">
                <span class="muted" data-i18n="loadAccountsPrompt">Click "Load status" to list accounts.</span>
              </div>
            </div>
            <div class="actions">
              <button id="saveSettings" type="button" class="primary" data-i18n="save">Save settings</button>
            </div>
          </div>
        </section>
      </aside>
      <section class="main">
        <div id="statsPanel" class="panel" style="display:none;">
          <h2><span class="dot"></span><span data-i18n="resetStats">Reset Statistics</span></h2>
          <div id="statsContent"></div>
        </div>
        <div id="accounts">
          <div class="panel empty" data-i18n="loadPrompt">Enter the CPA management key and click Load status.</div>
        </div>
        <div class="panel">
          <h2><span class="dot"></span><span data-i18n="logs">Logs</span></h2>
          <div id="logs" class="log"><span class="muted">-</span></div>
        </div>
      </section>
    </div>
  </div>
  <script>
    const I18N = {
      en: {
        title: "Codex Auto Reset",
        connection: "Connection",
        settings: "Settings",
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
        weeklyProgress: "Weekly quota",
        nextExpiry: "Next credit expiry",
        nextState: "Next",
        lastCycle: "Last cycle",
        noAccounts: "No accounts enabled.",
        neverExpires: "never expires",
        expired: "expired",
        resetStats: "Reset Statistics",
        totalSuccess: "Total success",
        totalFail: "Total fail",
        recovered: "Recovered quota",
        thisWeek: "This week",
        settingsSaved: "Settings saved.",
        confirmReset: "Force a reset cycle for this account now?",
        keyRequired: "Management key is required.",
        times: ""
      },
      zh: {
        title: "Codex \u81ea\u52a8\u91cd\u7f6e",
        connection: "\u8fde\u63a5",
        settings: "\u8bbe\u7f6e",
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
        weeklyProgress: "\u5468\u989d\u5ea6",
        nextExpiry: "\u4e0b\u4e00\u4e2a\u4fe1\u7528\u989d\u5ea6\u8fc7\u671f",
        nextState: "\u4e0b\u4e00\u8f6e",
        lastCycle: "\u4e0a\u4e00\u8f6e",
        noAccounts: "\u672a\u542f\u7528\u4efb\u4f55\u8d26\u53f7\u3002",
        neverExpires: "\u6c38\u4e0d\u8fc7\u671f",
        expired: "\u5df2\u8fc7\u671f",
        resetStats: "\u91cd\u7f6e\u7edf\u8ba1",
        totalSuccess: "\u7d2f\u8ba1\u6210\u529f",
        totalFail: "\u7d2f\u8ba1\u5931\u8d25",
        recovered: "\u6062\u590d\u989d\u5ea6",
        thisWeek: "\u672c\u5468\u6210\u529f",
        settingsSaved: "\u8bbe\u7f6e\u5df2\u4fdd\u5b58\u3002",
        confirmReset: "\u7acb\u5373\u5bf9\u8be5\u8d26\u53f7\u89e6\u53d1\u4e00\u6b21\u91cd\u7f6e\u6d41\u7a0b\uff1f",
        keyRequired: "\u9700\u8981\u7ba1\u7406\u5bc6\u94a5\u3002",
        times: "\u6b21"
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
      const loc = document.getElementById('locale').value;
      for (const btn of document.querySelectorAll('#localeSwitch button')) {
        if (btn.dataset.locale === loc) { btn.classList.add('active'); }
        else { btn.classList.remove('active'); }
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
    // describeNextAction renders the "next" line based on FSM state.
    // renderCardHistory renders the recent reset records for one account card.
    function renderCardHistory(records) {
      if (!records || records.length === 0) return '';
      var zh = document.getElementById('locale').value === 'zh';
      var lines = records.map(function (r) {
        var icon = r.success ? '\u2705' : '\u274c';
        var time = fmtTime(r.timestamp);
        var quota = (r.pre_weekly_pct != null && r.post_weekly_pct != null)
          ? r.pre_weekly_pct + '%\u2192' + r.post_weekly_pct + '%'
          : '\u2014';
        var label = zh ? '\u91cd\u7f6e' : 'reset';
        return '<div class="history-line">' + icon + ' ' + escapeHTML(time) + ' ' + escapeHTML(label) + ': ' + escapeHTML(quota) + '</div>';
      }).join('');
      return '<div class="history">' +
        '<span class="k">' + escapeHTML(zh ? '\u91cd\u7f6e\u5386\u53f2' : 'Reset history') + '</span>' +
        lines + '</div>';
    }

    // renderStats renders the top statistics panel from reset_history.
    function renderStats(history) {
      var panel = document.getElementById('statsPanel');
      var content = document.getElementById('statsContent');
      if (!history || history.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = 'block';

      var totalSuccess = 0, totalFail = 0, totalRecovered = 0;
      var now = new Date();
      var weekAgo = now.getTime() - 7 * 86400000;
      var weekSuccess = 0;

      history.forEach(function (r) {
        if (r.success) {
          totalSuccess++;
          var recovered = r.post_weekly_pct - r.pre_weekly_pct;
          if (recovered > 0) totalRecovered += recovered;
          if (new Date(r.timestamp).getTime() >= weekAgo) weekSuccess++;
        } else {
          totalFail++;
        }
      });

      var zh = document.getElementById('locale').value === 'zh';
      var unit = zh ? '\u6b21' : '';
      var html = '<div class="stats-grid">' +
        '<div class="stat success"><div class="stat-label">' + escapeHTML(t('totalSuccess')) + '</div>' +
        '<div class="stat-value">' + totalSuccess + '</div>' +
        '<div class="stat-sub">' + escapeHTML(t('thisWeek')) + ': ' + weekSuccess + unit + '</div></div>' +
        '<div class="stat danger"><div class="stat-label">' + escapeHTML(t('totalFail')) + '</div>' +
        '<div class="stat-value">' + totalFail + '</div>' +
        '<div class="stat-sub">&nbsp;</div></div>' +
        '<div class="stat primary"><div class="stat-label">' + escapeHTML(t('recovered')) + '</div>' +
        '<div class="stat-value">+' + totalRecovered + '%</div>' +
        '<div class="stat-sub">&nbsp;</div></div>' +
        '<div class="stat"><div class="stat-label">' + escapeHTML(t('resetStats')) + '</div>' +
        '<div class="stat-value">' + history.length + '</div>' +
        '<div class="stat-sub">' + escapeHTML(zh ? '\u603b\u8bb0\u5f55' : 'total records') + '</div></div>' +
        '</div>';
      content.innerHTML = html;
    }

    function describeNextAction(state, nextAt) {
      const zh = document.getElementById('locale').value === 'zh';
      const time = nextAt && !nextAt.startsWith('0001-') ? fmtTime(nextAt) : '';
      switch (state) {
        case 'IDLE':
          return zh
            ? (time ? '\u4e0b\u4e00\u8f6e\u68c0\u67e5\u5c06\u5728 ' + time + ' \u53d1\u751f\uff08\u5de1\u67e5\u91cd\u7f6e\u6b21\u6570\uff09' : '\u7b49\u5f85\u8c03\u5ea6')
            : (time ? 'next patrol at ' + time + ' (check credits)' : 'pending');
        case 'ARMED':
          return zh
            ? (time ? '\u5c06\u5728 ' + time + ' \u89e6\u53d1\u91cd\u7f6e\uff08\u8fdb\u5165\u786e\u8ba4\u6d41\u7a0b\uff09' : '\u5373\u5c06\u89e6\u53d1')
            : (time ? 'trigger at ' + time + ' (enter confirm)' : 'imminent');
        case 'CONFIRMING':
          return zh ? '\u6b63\u5728\u4e8c\u6b21\u786e\u8ba4\uff0c\u51c6\u5907\u53d1\u9001\u91cd\u7f6e\u8bf7\u6c42' : 'confirming before reset';
        case 'RESETTING':
          return zh ? '\u5df2\u53d1\u9001\u91cd\u7f6e\u8bf7\u6c42\uff0c\u7b49\u5f85\u54cd\u5e94' : 'reset request sent, awaiting response';
        case 'VERIFYING':
          return zh ? (time ? '\u5c06\u5728 ' + time + ' \u9a8c\u8bc1\u91cd\u7f6e\u7ed3\u679c' : '\u6b63\u5728\u9a8c\u8bc1') : (time ? 'verify at ' + time : 'verifying');
        case 'DONE':
          return zh ? '\u672c\u8f6e\u5b8c\u6210\uff0c\u5373\u5c06\u6062\u590d\u5de1\u67e5' : 'cycle complete, resuming patrol';
        default:
          return zh ? '\u672a\u77e5\u72b6\u6001' : 'unknown';
      }
    }

    function renderAccounts(status, logEntries, resetHistory) {
      // Build per-account reset history lookup (most recent 3 per account).
      var historyByAcct = {};
      (resetHistory || []).forEach(function (r) {
        if (!r.auth_id) return;
        if (!historyByAcct[r.auth_id]) historyByAcct[r.auth_id] = [];
        historyByAcct[r.auth_id].push(r);
      });
      // Sort each account's history descending by time, keep top 3.
      Object.keys(historyByAcct).forEach(function (k) {
        historyByAcct[k].sort(function (a, b) {
          return new Date(b.timestamp) - new Date(a.timestamp);
        });
        historyByAcct[k] = historyByAcct[k].slice(0, 3);
      });
      const box = document.getElementById('accounts');
      const accts = (status && status.accounts) || {};
      const ids = Object.keys(accts);
      if (ids.length === 0) {
        box.innerHTML = '<div class="panel empty">' + escapeHTML(t('noAccounts')) + '</div>';
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
        const weeklyPct = (typeof snap.weekly_pct === 'number' && snap.weekly_pct >= 0) ? snap.weekly_pct : null;
        const weekly = weeklyPct == null ? '-' : weeklyPct + '%';
        const nextAt = a.next_wake || '';
        const displayName = escapeHTML(accountNameMap[id] || id);

        // Weekly progress bar.
        var fillClass = 'unknown';
        var fillWidth = 0;
        if (weeklyPct != null) {
          fillWidth = Math.max(0, Math.min(100, weeklyPct));
          if (weeklyPct >= 50) fillClass = '';
          else if (weeklyPct >= 20) fillClass = 'warn';
          else fillClass = 'danger';
        }
        var progressHtml =
          '<div class="progress">' +
          '<div class="progress-head"><span class="k">' + escapeHTML(t('weeklyProgress')) + '</span>' +
          '<span class="v">' + escapeHTML(weekly) + '</span></div>' +
          '<div class="progress-track"><div class="progress-fill ' + fillClass + '" style="width:' + fillWidth + '%;"></div></div>' +
          '</div>';

        // Credit list: ID + YYYY-MM-DD HH:MM:SS + remaining + target marker.
        let creditListHtml = '';
        if (snap.credits && snap.credits.length > 0) {
          const sorted = snap.credits.slice().sort(function (x, y) {
            const xe = x.expires_at || '9999', ye = y.expires_at || '9999';
            return xe.localeCompare(ye);
          });
          creditListHtml = '<div class="credit-list">' + sorted.map(function (c, idx) {
            const cid = escapeHTML(shortCreditID(c.id));
            const expiryStr = c.expires_at ? fmtExpiryCompact(c.expires_at) : '\u2014';
            const remain = c.expires_at ? fmtRemainLocalized(c.expires_at) : t('neverExpires');
            const isTarget = idx === 0 ? ' <span class="muted" style="font-size:10px;">\u2190 target</span>' : '';
            return '<div class="credit-line">\u2022 <span class="cid">' + cid + '</span> ' +
              '<span class="muted">' + escapeHTML(expiryStr) + '</span> ' +
              escapeHTML(remain) + isTarget + '</div>';
          }).join('') + '</div>';
        } else {
          creditListHtml = '<div class="credit-list"><div class="credit-line muted">\u2014</div></div>';
        }

        // Mini stat tiles.
        var tilesHtml =
          '<div class="card-row">' +
          '<div class="mini-stat"><div class="k">' + escapeHTML(t('creditsAvail')) + '</div>' +
          '<div class="v">' + escapeHTML(String(credits)) + '</div></div>' +
          '<div class="mini-stat"><div class="k">' + escapeHTML(t('weeklyRemain')) + '</div>' +
          '<div class="v">' + escapeHTML(weekly) + '</div></div>' +
          '</div>';

        // Last log line for this account.
        const lastLog = lastLogByScope[id];
        const lastMsg = lastLog ? escapeHTML(translateLog(lastLog.message)) : '\u2014';
        // Determine next action description based on state.
        const nextDesc = describeNextAction(state, nextAt);

        // Combined last+next panel with tinted background.
        const statusPanel =
          '<div class="next" data-next="' + escapeHTML(nextAt) + '">' +
          '<div class="row"><span class="k">' + escapeHTML(t('lastCycle')) + ':</span> ' + lastMsg + '</div>' +
          '<div class="row"><span class="k">' + escapeHTML(t('nextState')) + ':</span> ' + escapeHTML(nextDesc) +
          ' <span class="countdown countdown-text muted"></span></div>' +
          '</div>';

        return '<div class="acct-card">' +
          '<div class="card-head"><span class="auth-id">' + displayName + '</span>' +
          '<span class="state-badge ' + escapeHTML(state) + '">' + escapeHTML(state) + '</span></div>' +
          progressHtml +
          tilesHtml +
          creditListHtml +
          statusPanel +
          renderCardHistory(historyByAcct[id]) +
          '<div class="card-actions">' +
          '<button class="check-btn primary" data-auth="' + escapeHTML(id) + '">' + escapeHTML(t('check')) + '</button>' +
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
      if (!iso || iso.startsWith('0001-')) return '\u2014';
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
    // "RateLimitResetCredit_d7087f83469c819182a87d5916512c9c" -> "d7087f83..."
    function shortCreditID(id) {
      if (!id) return '';
      const prefix = 'RateLimitResetCredit_';
      var s = id.startsWith(prefix) ? id.substring(prefix.length) : id;
      return s.length > 10 ? s.substring(0, 8) + '\u2026' : s;
    }

    // fmtRemainLocalized renders remaining time in the selected language:
    // ZH: "8 days 9 hours", EN: "8d 9h".
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
      const dayUnit = zh ? '\u5929' : 'd';
      const hourUnit = zh ? '\u5c0f\u65f6' : 'h';
      const minUnit = zh ? '\u5206' : 'm';
      const parts = [];
      if (days > 0) parts.push(days + dayUnit);
      if (hours > 0) parts.push(hours + hourUnit);
      if (days === 0 && hours === 0 && mins > 0) parts.push(mins + minUnit);
      return parts.join(zh ? '' : ' ') || (zh ? '\u4e0d\u52301\u5206' : '<1m');
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
        [/no available credits/g, '\u672a\u53d1\u73b0\u53ef\u7528\u91cd\u7f6e\u6b21\u6570'],
        [/credit ([a-f0-9\u2026]*) expires in ([^;]+); not near trigger window \(will arm ([^)]+) before expiry[^)]*\)/g,
         '\u91cd\u7f6e\u6b21\u6570 $1 \u5c06\u5728 $2 \u540e\u8fc7\u671f\uff1b\u5c1a\u672a\u8fdb\u5165\u89e6\u53d1\u7a97\u53e3\uff08\u5c06\u5728\u8fc7\u671f\u524d $3 \u8fdb\u5165 ARMED\uff09'],
        [/credit ([a-f0-9\u2026]*) expires in ([^;]+); not near trigger window.*/g,
         '\u91cd\u7f6e\u6b21\u6570 $1 \u5c06\u5728 $2 \u540e\u8fc7\u671f\uff1b\u5c1a\u672a\u8fdb\u5165\u89e6\u53d1\u7a97\u53e3'],
        [/credit ([a-f0-9\u2026]*) armed; will reset at ([^ ]+) \(expiry in ([^)]+)\)/g,
         '\u91cd\u7f6e\u6b21\u6570 $1 \u5df2\u8fdb\u5165 ARMED\uff1b\u5c06\u5728 $2 \u89e6\u53d1\u91cd\u7f6e\uff08\u8fc7\u671f\u524d\u8fd8\u6709 $3\uff09'],
        [/trigger time reached, confirming before reset/g, '\u89e6\u53d1\u65f6\u95f4\u5df2\u5230\uff0c\u6b63\u5728\u786e\u8ba4\u540e\u91cd\u7f6e'],
        [/confirmed target credit ([a-f0-9\u2026]*) \(weekly was (\d+%%)\); sending reset request/g,
         '\u5df2\u786e\u8ba4\u76ee\u6807\u91cd\u7f6e\u6b21\u6570 $1\uff08\u5468\u989d\u5ea6 $2\uff09\uff1b\u6b63\u5728\u53d1\u9001\u91cd\u7f6e\u8bf7\u6c42'],
        [/reset request accepted \(code=([^,]+), windows_reset=(\d+)\); will verify in ([^)]+)\)/g,
         '\u91cd\u7f6e\u8bf7\u6c42\u5df2\u63a5\u53d7\uff08code=$1\uff0c\u91cd\u7f6e\u7a97\u53e3=$2\uff09\uff1b$3 \u540e\u9a8c\u8bc1'],
        [/reset stopped: server returned (.+)/g, '\u91cd\u7f6e\u5df2\u505c\u6b62\uff1a\u670d\u52a1\u7aef\u8fd4\u56de $1'],
        [/reset stopped: (.+)/g, '\u91cd\u7f6e\u5df2\u505c\u6b62\uff1a$1'],
        [/reset failed \(attempt (\d+)\): (.+); retrying with same idempotency key/g,
         '\u91cd\u7f6e\u5931\u8d25\uff08\u7b2c $1 \u6b21\uff09\uff1a$2\uff1b\u4f7f\u7528\u76f8\u540c\u5e42\u7b49\u952e\u91cd\u8bd5'],
        [/retries exhausted after (\d+) attempts; last error: (.+)/g,
         '\u91cd\u8bd5 $1 \u6b21\u540e\u653e\u5f03\uff1b\u6700\u540e\u9519\u8bef\uff1a$2'],
        [/reset verified: credits (\d+)\u2192(\d+), weekly (\d+%%)\u2192(\d+%%)/g,
         '\u91cd\u7f6e\u9a8c\u8bc1\u901a\u8fc7\uff1a\u91cd\u7f6e\u6b21\u6570 $1\u2192$2\uff0c\u5468\u989d\u5ea6 $3\u2192$4'],
        [/reset partial: credits ok but weekly (\d+%%)\u2192(\d+%%) \(delayed\?\)/g,
         '\u91cd\u7f6e\u90e8\u5206\u5b8c\u6210\uff1a\u6b21\u6570\u5df2\u6263\u4f46\u5468\u989d\u5ea6 $1\u2192$2\uff08\u5ef6\u8fdf\uff1f\uff09'],
        [/verification mismatch: target gone=(\w+), count-1=(\w+), quota up=(\w+) \u2014 halted/g,
         '\u9a8c\u8bc1\u4e0d\u5339\u914d\uff1a\u76ee\u6807\u5df2\u6263=$1\uff0c\u6b21\u6570-1=$2\uff0c\u989d\u5ea6\u56de\u5347=$3 \u2014\u2014 \u5df2\u505c\u6b62'],
        [/verification failed: (.+)/g, '\u9a8c\u8bc1\u5931\u8d25\uff1a$1'],
        [/target credit vanished before reset/g, '\u76ee\u6807\u91cd\u7f6e\u6b21\u6570\u5728\u91cd\u7f6e\u524d\u5df2\u6d88\u5931'],
        [/armed wake at T, confirming/g, '\u89e6\u53d1\u65f6\u95f4\u5df2\u5230\uff0c\u6b63\u5728\u786e\u8ba4'],
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
    // so the right-panel cards can show "codex-44411af1-...-team.json" instead
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
        renderAccounts(status, logsResp.entries || [], status.reset_history || []);
        renderStats(status.reset_history || []);
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
        box.innerHTML = '<span class="muted">' + escapeHTML(t('noAccountsFound')) + '</span>';
        return;
      }
      box.innerHTML = accts.map(function (a) {
        var checked = a.enabled ? ' checked' : '';
        // a.name is already the most informative label (ID/file name preferred).
        var label = escapeHTML(a.name || a.email || a.auth_index);
        return '<label class="acct-item">' +
               '<input type="checkbox" class="acct-checkbox" value="' + escapeHTML(a.auth_index) + '"' + checked + '>' +
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
      const savedLocale = localStorage.getItem('codex-auto-reset-locale');
      if (savedLocale === 'zh' || savedLocale === 'en') {
        document.getElementById('locale').value = savedLocale;
      } else {
        document.getElementById('locale').value = (navigator.language || '').toLowerCase().indexOf('zh') === 0 ? 'zh' : 'en';
      }
      document.getElementById('locale').addEventListener('change', function () {
        localStorage.setItem('codex-auto-reset-locale', document.getElementById('locale').value);
        applyI18n();
      });
      // Locale switch buttons drive the hidden <select> so all listeners stay wired.
      document.getElementById('localeSwitch').addEventListener('click', function (ev) {
        const target = ev.target;
        if (!(target instanceof Element) || target.tagName !== 'BUTTON') return;
        document.getElementById('locale').value = target.dataset.locale || 'en';
        localStorage.setItem('codex-auto-reset-locale', document.getElementById('locale').value);
        applyI18n();
      });
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
