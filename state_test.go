package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestState_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	nextWake := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	s := &PluginState{
		Config: Config{RefreshInterval: 8 * time.Hour, TriggerLeadTime: 4 * time.Hour, EnabledAccounts: []string{"a"}},
		Accounts: map[string]AccountRuntime{
			"a": {AuthID: "a", State: StateARMED, NextWake: nextWake},
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
	if !loaded.Accounts["a"].NextWake.Equal(nextWake) {
		t.Fatalf("NextWake not preserved: %v vs %v", loaded.Accounts["a"].NextWake, nextWake)
	}
}

func TestState_LoadMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	loaded, err := loadState(filepath.Join(dir, "nonexistent.json"))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if loaded == nil {
		t.Fatalf("loaded is nil")
	}
	if loaded.Accounts == nil {
		t.Fatalf("Accounts map is nil")
	}
	if len(loaded.Accounts) != 0 {
		t.Fatalf("Accounts not empty: %v", loaded.Accounts)
	}
}

func TestState_SaveCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "state.json")
	s := &PluginState{Config: DefaultConfig(), Accounts: map[string]AccountRuntime{}}
	if err := saveState(nested, s); err != nil {
		t.Fatalf("save with nested dir: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestState_SaveIsAtomicViaTmpRename(t *testing.T) {
	// If save is interrupted mid-write, the existing file must remain intact.
	// We verify atomicity indirectly: a successful save leaves a complete file
	// and no .tmp leftover.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := &PluginState{Config: DefaultConfig(), Accounts: map[string]AccountRuntime{}}
	if err := saveState(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected no .tmp leftover, got %v", err)
	}
}

// TestDefaultStatePath_IsAbsoluteAndPlatformAppropriate verifies the state file
// lives under the OS user-config dir (not CWD) so it survives CPA reinstalls:
// Windows %APPDATA%, Linux ~/.config, macOS ~/Library/Application Support.
func TestDefaultStatePath_IsAbsoluteAndPlatformAppropriate(t *testing.T) {
	p := defaultStatePath()
	if !filepath.IsAbs(p) {
		t.Fatalf("state path should be absolute, got %q (UserConfigDir unavailable in test env?)", p)
	}
	// Must contain the plugin-specific segment so multiple CPA plugins don't collide.
	if !strings.Contains(p, filepath.Join("CLIProxyAPI", "codex-auto-reset")) {
		t.Fatalf("state path missing plugin dir segment: %q", p)
	}
	if filepath.Base(p) != "state.json" {
		t.Fatalf("unexpected file name: %q", filepath.Base(p))
	}
}

// TestMigrateLegacyState_MigratesAndKeepsBackup verifies the one-time migration
// from the legacy CWD-relative state file to the new config-dir location.
func TestMigrateLegacyState_MigratesAndKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	// Create a legacy state file in CWD (chdir to temp dir for isolation).
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	legacy := &PluginState{
		Config:       DefaultConfig(),
		Accounts:     map[string]AccountRuntime{},
		ResetHistory: []ResetRecord{{Timestamp: time.Now(), AuthID: "a", Success: true, PreWeeklyPct: 10, PostWeeklyPct: 100}},
	}
	if err := saveState("codex-auto-reset.state.json", legacy); err != nil {
		t.Fatalf("save legacy: %v", err)
	}

	newPath := filepath.Join(dir, "new", "state.json")
	migrateLegacyState(newPath)

	// New location must contain the data.
	got, err := loadState(newPath)
	if err != nil {
		t.Fatalf("load migrated: %v", err)
	}
	if len(got.ResetHistory) != 1 || !got.ResetHistory[0].Success {
		t.Fatalf("migration lost reset history: %+v", got.ResetHistory)
	}
	// Legacy file must be renamed to .migrated (kept, not deleted).
	if _, err := os.Stat("codex-auto-reset.state.json.migrated"); err != nil {
		t.Fatalf("legacy backup missing: %v", err)
	}
}

// TestMigrateLegacyState_DoesNotOverwriteExisting verifies migration is a
// no-op when the new location already has state (e.g. re-running an upgrade).
func TestMigrateLegacyState_DoesNotOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	// Legacy file with 2 records.
	legacy := &PluginState{ResetHistory: []ResetRecord{{AuthID: "legacy"}, {AuthID: "legacy2"}}}
	if err := saveState("codex-auto-reset.state.json", legacy); err != nil {
		t.Fatalf("save legacy: %v", err)
	}
	// New location with 1 record already.
	newPath := filepath.Join(dir, "new", "state.json")
	existing := &PluginState{ResetHistory: []ResetRecord{{AuthID: "new"}}}
	if err := saveState(newPath, existing); err != nil {
		t.Fatalf("save existing: %v", err)
	}

	migrateLegacyState(newPath)

	got, err := loadState(newPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.ResetHistory) != 1 || got.ResetHistory[0].AuthID != "new" {
		t.Fatalf("existing state was overwritten: %+v", got.ResetHistory)
	}
}
