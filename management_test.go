package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testMgmtHelper struct {
	h *managementHandlers
}

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
	status, body := m.h.handle(http.MethodGet, stripPrefix(path), nil, nil)
	if status >= 400 {
		t.Fatalf("GET %s: status %d body %s", path, status, body)
	}
	return body
}

func (m *testMgmtHelper) put(t *testing.T, path string, b []byte) []byte {
	t.Helper()
	status, body := m.h.handle(http.MethodPut, stripPrefix(path), nil, b)
	if status >= 400 {
		t.Fatalf("PUT %s: status %d body %s", path, status, body)
	}
	return body
}

func (m *testMgmtHelper) post(t *testing.T, path string, b []byte) []byte {
	t.Helper()
	status, body := m.h.handle(http.MethodPost, stripPrefix(path), nil, b)
	if status >= 400 {
		t.Fatalf("POST %s: status %d body %s", path, status, body)
	}
	return body
}

func stripPrefix(path string) string {
	const pfx = "/v0/management/plugins/codex-auto-reset"
	return strings.TrimPrefix(path, pfx)
}

func TestManagement_StatusRoute(t *testing.T) {
	srv := newTestManagement(t, Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour, EnabledAccounts: []string{"a"}}, []string{"a"})
	body := srv.get(t, "/v0/management/plugins/codex-auto-reset/status")
	var resp struct {
		Plugin   string                    `json:"plugin"`
		Config   Config                    `json:"config"`
		Accounts map[string]AccountRuntime `json:"accounts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Plugin != "codex-auto-reset" {
		t.Fatalf("plugin = %q", resp.Plugin)
	}
	if resp.Config.RefreshInterval != 12*time.Hour {
		t.Fatalf("R = %v", resp.Config.RefreshInterval)
	}
	if _, ok := resp.Accounts["a"]; !ok {
		t.Fatalf("account 'a' missing: %v", resp.Accounts)
	}
}

func TestManagement_PutSettingsUpdatesAndPersists(t *testing.T) {
	srv := newTestManagement(t, DefaultConfig(), nil)
	body := srv.put(t, "/v0/management/plugins/codex-auto-reset/settings", []byte(`{"refresh_interval":"4h","trigger_lead_time":"2h","enabled_accounts":["x"]}`))
	var resp struct {
		RefreshInterval string   `json:"refresh_interval"`
		TriggerLeadTime string   `json:"trigger_lead_time"`
		EnabledAccounts []string `json:"enabled_accounts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.RefreshInterval != "4h" || resp.TriggerLeadTime != "2h" {
		t.Fatalf("echo = %+v", resp)
	}
	if len(resp.EnabledAccounts) != 1 || resp.EnabledAccounts[0] != "x" {
		t.Fatalf("enabled = %v", resp.EnabledAccounts)
	}
	// State file should now exist (persisted).
	loaded, err := loadState(srv.h.statePath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Config.RefreshInterval != 4*time.Hour {
		t.Fatalf("persisted R = %v", loaded.Config.RefreshInterval)
	}
}

func TestManagement_UnknownRouteReturns404(t *testing.T) {
	srv := newTestManagement(t, DefaultConfig(), nil)
	status, _ := srv.h.handle(http.MethodGet, "/codex-auto-reset/nonexistent", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestManagement_ImportExportRoundTrip(t *testing.T) {
	srv := newTestManagement(t, DefaultConfig(), nil)
	exported := srv.get(t, "/v0/management/plugins/codex-auto-reset/export")
	// Re-import the exported state.
	srv.post(t, "/v0/management/plugins/codex-auto-reset/import", exported)
	// State should be unchanged (round-trip).
	loaded, _ := loadState(srv.h.statePath)
	if loaded.Config.RefreshInterval != DefaultConfig().RefreshInterval {
		t.Fatalf("import changed config: R = %v", loaded.Config.RefreshInterval)
	}
}

func TestManagement_ImportRejectsBadJSON(t *testing.T) {
	srv := newTestManagement(t, DefaultConfig(), nil)
	status, _ := srv.h.handle(http.MethodPost, "/codex-auto-reset/import", nil, []byte(`{bad json`))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestStatusHTML_ContainsBilingualMarkers(t *testing.T) {
	h := newManagementHandlers(&PluginState{Config: DefaultConfig(), Accounts: map[string]AccountRuntime{}}, "", nil)
	status, body := h.handle(http.MethodGet, "codex-auto-reset/resource/status", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	html := string(body)
	for _, want := range []string{"codex-auto-reset", "data-i18n", "EN", "ZH", "fetch(", "authorization"} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML missing %q", want)
		}
	}
}
