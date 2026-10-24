package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static int call_host_api(cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	hostAPI        atomic.Pointer[C.cliproxy_host_api]
	pluginStateMu  sync.Mutex
	globalState    = &PluginState{Accounts: map[string]AccountRuntime{}}
	globalWorker   *Worker
	globalHandlers *managementHandlers
	// globalStatePath is the state file resolved once per boot in
	// configurePlugin (explicit state_dir > CPA auth dir > OS user-config
	// dir). All saves go through it so reconfigure cannot split state
	// across locations.
	globalStatePath string
	pluginVersion   = "0.1.13"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI.Store(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pluginStateMu.Lock()
	w := globalWorker
	pluginStateMu.Unlock()
	if w != nil {
		w.Stop()
	}
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHostCallbackABI is the bridge from Go to the host process. It marshals
// a payload, invokes the C host-call helper, decodes the envelope, and
// returns the `result` field. Errors are returned for any non-OK envelope.
func callHostCallbackABI(method string, payload any) (json.RawMessage, error) {
	host := hostAPI.Load()
	if host == nil {
		return nil, fmt.Errorf("host callback %s unavailable", method)
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, err)
	}

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	callCode := C.call_host_api(host, cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(host, response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// --- Section C: dispatch + credential loader ---

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type managementRequest struct {
	pluginapi.ManagementRequest
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistrationResponse())
	case pluginabi.MethodManagementHandle:
		return okEnvelope(handleManagementRequest(request))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configurePlugin(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := DefaultConfig()
	if len(req.ConfigYAML) > 0 {
		parsed, err := parseConfigYAML(req.ConfigYAML)
		if err != nil {
			return err
		}
		cfg = parsed
	}

	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	// Resolve the state location ONCE per boot, most-durable first (see
	// resolveStatePath). Container deployments volume-mount CPA's auth dir
	// but rarely the OS user-config dir, so keying state off the auth dir is
	// what makes it survive image updates and container recreates.
	yamlStateDir := strings.TrimSpace(cfg.StateDir)
	authEntries, authListErr := listAuthsViaHost()
	statePath := resolveStatePath(yamlStateDir, authEntries)
	globalStatePath = statePath
	// Forward migrations from superseded locations. Each runs at most once;
	// sources are kept as .migrated, never destroyed.
	migrateLegacyState(statePath)                   // pre-0.1.7 CWD-relative file
	migrateStateFile(defaultStatePath(), statePath) // 0.1.7+ OS user-config dir
	loaded, source, err := loadState(statePath)
	if err != nil {
		// Start empty but treat persistence as suspect: log loudly so the
		// operator can inspect the file before the next save replaces it.
		source = "error: " + err.Error()
		loaded = &PluginState{Accounts: map[string]AccountRuntime{}}
	}
	if loaded.Config.RefreshInterval == 0 && loaded.Config.TriggerLeadTime == 0 && len(loaded.Config.EnabledAccounts) == 0 {
		// Fresh state: use the config we just parsed.
		loaded.Config = cfg
	} else {
		cfg = loaded.Config
	}
	// The config_yaml override is authoritative for state_dir across
	// state.json round-trips (a fresh save persists it for later boots).
	if yamlStateDir != "" {
		cfg.StateDir = yamlStateDir
	}
	globalState = loaded
	globalState.Config = cfg

	// Stop any prior worker, start a fresh one with the resolved config + accounts.
	if globalWorker != nil {
		globalWorker.Stop()
	}
	enabled := cfg.EnabledAccounts
	globalWorker = NewWorker(cfg, enabled, hostCredsLoader, time.Now)
	// Always announce the state location and load source — on FIRST load too,
	// not just reconfigure — so operators know what to back up / volume-mount.
	loadLevel := "info"
	if strings.HasPrefix(source, "error") {
		loadLevel = "error"
	} else if strings.Contains(source, "corrupt") {
		loadLevel = "warn"
	}
	globalWorker.Logs().append(LogEntry{
		Timestamp: time.Now(), Level: loadLevel, Scope: "system",
		Message: "state file: " + statePath + " (loaded: " + source + ")",
	})
	if authListErr != nil {
		globalWorker.Logs().append(LogEntry{
			Timestamp: time.Now(), Level: "warn", Scope: "system",
			Message: "auth list for state-dir derivation failed: " + authListErr.Error() + "; state may not survive container recreates",
		})
	}
	// Restore persisted logs into the ring so they survive CPA restarts.
	if len(loaded.Logs) > 0 {
		globalWorker.Logs().load(loaded.Logs)
	}
	// Backfill reset history from logs if not already in ResetHistory.
	// This ensures past resets (from before the history feature existed)
	// are counted in the statistics panel.
	backfillResetHistoryFromLogs(loaded)
	// Wire StateSync so the management UI sees live FSM state and a restart
	// resumes mid-cycle. Debounce persistence: only write to disk at most every
	// stateSyncPersistInterval to avoid hammering the file on tight retry loops.
	globalWorker.StateSync = func(authID string, fsm *AccountFSM) {
		rec := syncFSMIntoGlobalState(globalWorker.Logs(), authID, fsm)
		afterVerifiedReset(globalWorker.Logs(), authID, rec)
		debouncedSaveState(statePath)
	}
	go globalWorker.Run()

	if globalHandlers == nil {
		globalHandlers = newManagementHandlers(globalState, statePath, globalWorker)
	} else {
		globalHandlers.state = globalState
		globalHandlers.statePath = statePath
		globalHandlers.worker = globalWorker
	}
	globalHandlers.lister = listCodexAccountsForUI
	globalHandlers.OnSettingsChanged = restartWorkerFromState
	return nil
}

// restartWorkerFromState stops the current worker (if any) and starts a fresh
// one using the current globalState.Config. Called on plugin load AND on every
// PUT /settings so config changes (enabled_accounts, refresh_interval) take
// effect immediately instead of requiring a plugin reload.
func restartWorkerFromState() {
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()
	if globalWorker != nil {
		globalWorker.Stop()
	}
	cfg := globalState.Config
	w := NewWorker(cfg, cfg.EnabledAccounts, hostCredsLoader, time.Now)
	// Carry over existing logs so a settings-change restart doesn't lose them.
	if globalWorker != nil {
		w.Logs().load(globalWorker.Logs().all())
	} else if len(globalState.Logs) > 0 {
		w.Logs().load(globalState.Logs)
	}
	w.StateSync = func(authID string, fsm *AccountFSM) {
		rec := syncFSMIntoGlobalState(w.Logs(), authID, fsm)
		afterVerifiedReset(w.Logs(), authID, rec)
		if globalStatePath == "" {
			globalStatePath = defaultStatePath() // defensive: never expected
		}
		debouncedSaveState(globalStatePath)
	}
	globalWorker = w
	if globalHandlers != nil {
		globalHandlers.worker = w
	}
	go w.Run()
}

// syncFSMIntoGlobalState mirrors one FSM snapshot into the persisted state and
// returns a pending reset-history record (if the step completed a cycle).
// Returns the record to the caller so host-side post-reset work can run
// outside pluginStateMu.
func syncFSMIntoGlobalState(logs *logRing, authID string, fsm *AccountFSM) *ResetRecord {
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()
	globalState.Accounts[authID] = AccountRuntime{
		AuthID:       authID,
		State:        fsm.State(),
		NextWake:     fsm.NextWake(),
		LastSnapshot: fsm.LastSnapshot(),
	}
	// Collect pending reset history from the FSM (set in stepVERIFYING,
	// not cleared by Reset).
	var rec *ResetRecord
	if r := fsm.TakePendingHistory(); r != nil {
		globalState.ResetHistory = append(globalState.ResetHistory, *r)
		// Cap at 500 entries (FIFO).
		if len(globalState.ResetHistory) > 500 {
			globalState.ResetHistory = globalState.ResetHistory[len(globalState.ResetHistory)-500:]
		}
		rec = r
	}
	// Also sync logs to the persisted state.
	globalState.Logs = logs.all()
	return rec
}

// afterVerifiedReset clears CPA's own model/credential cooldown for an
// account whose reset credit redemption was verified upstream. The plugin
// talks to OpenAI directly, so its quota view can show fresh capacity while
// CPA core still rejects routing until the old 429 cooldown's next_retry_after
// (quota-scheduler issue #14). Hosts predating the v8 plugin ABI reject the
// callback; that is logged and otherwise ignored so the reset still counts.
func afterVerifiedReset(logs *logRing, authID string, rec *ResetRecord) {
	if rec == nil || !rec.Success {
		return
	}
	resp, err := resetRoutingCooldown(authID)
	entry := LogEntry{Timestamp: time.Now(), Scope: authID, State: StateDONE}
	if err != nil {
		entry.Level = "warn"
		entry.Message = "reset verified, but clearing CPA core cooldown failed: " + err.Error()
	} else {
		entry.Level = "info"
		entry.Message = fmt.Sprintf("reset verified; cleared CPA core routing cooldown for %d model(s)", len(resp.Models))
	}
	logs.append(entry)
	pluginStateMu.Lock()
	globalState.Logs = logs.all()
	pluginStateMu.Unlock()
}

// resetRoutingCooldown is a variable so tests can stub the host ABI.
var resetRoutingCooldown = func(authID string) (pluginapi.HostRoutingResetCooldownResponse, error) {
	result, err := callHostCallbackABI(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authID})
	if err != nil {
		return pluginapi.HostRoutingResetCooldownResponse{}, err
	}
	var resp pluginapi.HostRoutingResetCooldownResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("decode host.routing.reset_cooldown result: %w", err)
	}
	return resp, nil
}

// listCodexAccountsForUI is the AccountLister wired into managementHandlers.
// It calls the host ABI to enumerate CPA's auth files and returns the Codex
// ones, sorted, for the UI's checkbox list.
func listCodexAccountsForUI() ([]AccountOption, error) {
	auths, err := listAuthsViaHost()
	if err != nil {
		return nil, err
	}
	opts := make([]AccountOption, 0, len(auths))
	for _, a := range auths {
		if !strings.EqualFold(a.Provider, "codex") && !strings.EqualFold(a.Type, "codex") {
			continue
		}
		// Skip only explicitly disabled accounts. Unavailable (rate-limited)
		// accounts are the ones that NEED resets, so they must be included —
		// the plugin talks to OpenAI directly, not through CPA's scheduler.
		if a.Disabled {
			continue
		}
		// Label preference: ID (the credential file name, e.g.
		// codex-44411af1-email-team.json — most informative), then Name,
		// then Email, then AuthIndex as last resort.
		label := a.ID
		if label == "" {
			label = a.Name
		}
		if label == "" {
			label = a.Email
		}
		if label == "" {
			label = a.AuthIndex
		}
		opts = append(opts, AccountOption{
			AuthIndex: a.AuthIndex,
			ID:        a.ID,
			Name:      label,
			Email:     a.Email,
			Account:   a.Account,
		})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })
	return opts, nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Codex Auto Reset",
			Version:          pluginVersion,
			Author:           "Jeffery",
			GitHubRepository: "https://github.com/JefferyZhang2019/cpa-plugin-codex-auto-reset",
		},
		Capabilities: registrationCapabilities{ManagementAPI: true},
	}
}

func managementRegistrationResponse() pluginapi.ManagementRegistrationResponse {
	resources := []pluginapi.ResourceRoute{{
		Path:        "/status",
		Menu:        "Codex Auto Reset",
		Description: "Auto-redeem Codex Reset Bank credits before they expire.",
	}}
	routes := []pluginapi.ManagementRoute{
		{Method: http.MethodGet, Path: "/plugins/codex-auto-reset/status"},
		{Method: http.MethodGet, Path: "/plugins/codex-auto-reset/accounts"},
		{Method: http.MethodGet, Path: "/plugins/codex-auto-reset/debug/auth-list"},
		{Method: http.MethodGet, Path: "/plugins/codex-auto-reset/debug/usage"},
		{Method: http.MethodGet, Path: "/plugins/codex-auto-reset/logs"},
		{Method: http.MethodPut, Path: "/plugins/codex-auto-reset/settings"},
		{Method: http.MethodPost, Path: "/plugins/codex-auto-reset/check"},
		{Method: http.MethodPost, Path: "/plugins/codex-auto-reset/check/all"},
		{Method: http.MethodPost, Path: "/plugins/codex-auto-reset/reset"},
		{Method: http.MethodGet, Path: "/plugins/codex-auto-reset/export"},
		{Method: http.MethodPost, Path: "/plugins/codex-auto-reset/import"},
	}
	return pluginapi.ManagementRegistrationResponse{Routes: routes, Resources: resources}
}

// handleManagementRequest dispatches a ManagementRequest to the handlers.
func handleManagementRequest(raw []byte) pluginapi.ManagementResponse {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "invalid management request: " + err.Error()})
	}
	pluginStateMu.Lock()
	h := globalHandlers
	pluginStateMu.Unlock()
	if h == nil {
		return mgmtJSONResponse(http.StatusServiceUnavailable, map[string]any{"error": "plugin not configured"})
	}
	status, body, contentType := h.handle(req.Method, req.Path, req.Headers, req.Body)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    managementResponseHeaders(contentType),
		Body:       body,
	}
}

// managementResponseHeaders builds the response headers for a plugin route.
// The management/resource page ships inside the binary, so a cached copy
// keeps serving a previous release's UI (and its API assumptions) after an
// upgrade; HTML is explicitly uncacheable.
func managementResponseHeaders(contentType string) http.Header {
	headers := http.Header{"Content-Type": []string{contentType}}
	if strings.HasPrefix(contentType, "text/html") {
		headers.Set("Cache-Control", "no-store, no-cache, must-revalidate")
		headers.Set("Pragma", "no-cache")
		headers.Set("Expires", "0")
	}
	return headers
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(v)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}

// hostCredsLoader is the CredsLoader passed to the worker. It calls the host
// ABI to (1) list auth files, (2) get the matching one's JSON, (3) extract
// Codex credentials. Returns an error if the account isn't found or isn't a
// Codex credential.
func hostCredsLoader(authID string) (CodexCredentials, ResetClient, error) {
	auths, err := listAuthsViaHost()
	if err != nil {
		return CodexCredentials{}, nil, fmt.Errorf("list auths: %w", err)
	}
	for _, a := range auths {
		if a.ID != authID && a.AuthIndex != authID {
			continue
		}
		if !strings.EqualFold(a.Provider, "codex") && !strings.EqualFold(a.Type, "codex") {
			return CodexCredentials{}, nil, fmt.Errorf("auth %s is not a codex credential (provider=%q type=%q)", authID, a.Provider, a.Type)
		}
		raw, err := getAuthJSONViaHost(a.AuthIndex)
		if err != nil {
			return CodexCredentials{}, nil, fmt.Errorf("get auth %s: %w", authID, err)
		}
		creds, err := ExtractCodexCredentials(raw)
		if err != nil {
			return CodexCredentials{}, nil, fmt.Errorf("extract credentials %s: %w", authID, err)
		}
		return creds, &OpenAIClient{}, nil
	}
	return CodexCredentials{}, nil, fmt.Errorf("auth %q not found", authID)
}

func listAuthsViaHost() ([]pluginapi.HostAuthFileEntry, error) {
	result, err := callHostCallbackABI(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	return resp.Files, nil
}

func getAuthJSONViaHost(authIndex string) (json.RawMessage, error) {
	result, err := callHostCallbackABI(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.get result: %w", err)
	}
	return resp.JSON, nil
}

func defaultStatePath() string {
	// Legacy default: the OS user config dir (e.g. %APPDATA%\CLIProxyAPI\
	// codex-auto-reset on Windows, ~/.config/CLIProxyAPI/codex-auto-reset/
	// on Linux). Inside containers this path is ephemeral unless explicitly
	// volume-mounted, which is why resolveStatePath prefers CPA's auth dir.
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "CLIProxyAPI", "codex-auto-reset", "state.json")
}

// resolveStatePath picks the state file location, most-durable first:
//
//  1. explicit absolute state_dir from the plugin's config_yaml (operator
//     override; CPA's config.yaml lives on a mounted volume in container
//     deployments, so the override itself is durable)
//  2. <auth-dir>/plugin-state/codex-auto-reset/state.json — CPA's auth dir,
//     derived from host.auth.list entry paths. Operators volume-mount the
//     auth dir (credentials must survive), so state here survives image
//     updates and container recreates. The subdirectory keeps plugin state
//     out of CPA's auth-file scan root.
//  3. <os-user-config-dir>/CLIProxyAPI/codex-auto-reset/state.json — the
//     0.1.7+ default (defaultStatePath)
//
// The fallback chain is also the migration chain: configurePlugin moves any
// existing state forward to the resolved primary exactly once.
func resolveStatePath(stateDir string, authEntries []pluginapi.HostAuthFileEntry) string {
	if dir := strings.TrimSpace(stateDir); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "state.json")
	}
	for _, e := range authEntries {
		if e.Path != "" && filepath.IsAbs(e.Path) {
			return filepath.Join(filepath.Dir(e.Path), "plugin-state", "codex-auto-reset", "state.json")
		}
	}
	return defaultStatePath()
}

// legacyStatePath is the pre-0.1.7 location: a relative filename resolved against
// CPA's working directory. migrateLegacyState moves it to the new location once,
// so statistics and logs survive the upgrade.
func migrateLegacyState(newPath string) {
	migrateStateFile("codex-auto-reset.state.json", newPath)
}

// backfillResetHistoryFromLogs scans persisted logs for "reset verified OK" /
// "PARTIAL" / "MISMATCH" entries and creates ResetRecords for any that aren't
// already in ResetHistory. This ensures past resets (from before the history
// feature was added) are counted in statistics. Deduplicates by timestamp+authID.
func backfillResetHistoryFromLogs(state *PluginState) {
	// Collapse duplicates recorded by earlier versions (same authID + creditID
	// + second), keeping the first occurrence. Runs even with no logs so stale
	// duplicates in an upgraded state file are cleaned up.
	collapseResetHistoryDuplicates(state)

	if len(state.Logs) == 0 {
		return
	}
	// Dedup key: authID + creditID + second-truncated timestamp. The history
	// record (from pendingHistory) and the log entry for the SAME reset carry
	// two separate time.Now() readings that differ by nanoseconds, so
	// nanosecond-precision keys never match and every restart appended a
	// duplicate. Second-granularity + creditID is exact enough: one credit is
	// consumed at most once per second, and credit IDs are unique.
	backfillKey := func(authID, creditID string, ts time.Time) string {
		return authID + "|" + creditID + "|" + ts.Truncate(time.Second).Format(time.RFC3339)
	}
	existing := make(map[string]bool, len(state.ResetHistory))
	for _, r := range state.ResetHistory {
		existing[backfillKey(r.AuthID, r.CreditID, r.Timestamp)] = true
	}

	for _, e := range state.Logs {
		// Only process DONE-state entries that mention "reset".
		if e.State != StateDONE {
			continue
		}
		details := e.Details
		if details == nil {
			continue
		}
		// Check if this log has reset verification data.
		preCredits, _ := details["pre_credits"].(float64)
		postCredits, _ := details["post_credits"].(float64)
		preWeekly, _ := details["pre_weekly_pct"].(float64)
		postWeekly, _ := details["post_weekly_pct"].(float64)
		creditID, _ := details["credit_id"].(string)

		// Skip if no meaningful data.
		if preCredits == 0 && postCredits == 0 && creditID == "" {
			continue
		}

		key := backfillKey(e.Scope, creditID, e.Timestamp)
		if existing[key] {
			continue
		}

		// Determine success from the log message.
		success := false
		if strings.Contains(e.Message, "verified OK") {
			success = true
		}

		state.ResetHistory = append(state.ResetHistory, ResetRecord{
			Timestamp:     e.Timestamp,
			AuthID:        e.Scope,
			CreditID:      creditID,
			Success:       success,
			PreWeeklyPct:  int(preWeekly),
			PostWeeklyPct: int(postWeekly),
			PreCredits:    int(preCredits),
			PostCredits:   int(postCredits),
		})
		existing[key] = true
	}

}

// collapseResetHistoryDuplicates removes records sharing (authID, creditID,
// second), keeping the first occurrence. Repairs state files polluted by the
// pre-0.1.8 nanosecond-dedup bug in backfill.
func collapseResetHistoryDuplicates(state *PluginState) {
	seen := make(map[string]bool, len(state.ResetHistory))
	deduped := state.ResetHistory[:0]
	for _, r := range state.ResetHistory {
		key := r.AuthID + "|" + r.CreditID + "|" + r.Timestamp.Truncate(time.Second).Format(time.RFC3339)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	state.ResetHistory = deduped
}

// stateSyncPersistInterval bounds how often StateSync writes to disk. The
// worker calls StateSync after every FSM step; without a debounce, a retry
// storm (5 retries over ~48 min) or a fast IDLE patrol loop could write the
// state file many times per second. 5s is well below any user-perceptible
// latency while keeping disk writes bounded.
const stateSyncPersistInterval = 5 * time.Second

var (
	lastStatePersistMu sync.Mutex
	lastStatePersist   time.Time
)

// debouncedSaveState writes state to disk at most once per
// stateSyncPersistInterval. Safe to call from the worker goroutine on every
// FSM step. A final write on shutdown is handled by SaveStateNow.
func debouncedSaveState(path string) {
	lastStatePersistMu.Lock()
	if time.Since(lastStatePersist) < stateSyncPersistInterval {
		lastStatePersistMu.Unlock()
		return
	}
	lastStatePersist = time.Now()
	lastStatePersistMu.Unlock()
	pluginStateMu.Lock()
	snapshot := globalState
	pluginStateMu.Unlock()
	_ = saveState(path, snapshot)
}

// SaveStateNow forces an immediate persist (used on shutdown / reconfigure).
func SaveStateNow(path string) error {
	lastStatePersistMu.Lock()
	lastStatePersist = time.Now()
	lastStatePersistMu.Unlock()
	pluginStateMu.Lock()
	snapshot := globalState
	pluginStateMu.Unlock()
	return saveState(path, snapshot)
}

var _ = strings.TrimSpace
