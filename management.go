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
