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
	loaded, _, err := loadState(path)
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
	loaded, _, err := loadState(filepath.Join(dir, "nonexistent.json"))
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
	got, _, err := loadState(newPath)
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

	got, _, err := loadState(newPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.ResetHistory) != 1 || got.ResetHistory[0].AuthID != "new" {
		t.Fatalf("existing state was overwritten: %+v", got.ResetHistory)
	}
}

// TestMigrateStateFile_MigratesOnceAndKeepsSource verifies the generic
// forward migration: data lands at the destination, the source is kept as
// .migrated, and a second run cannot overwrite an existing destination.
func TestMigrateStateFile_MigratesOnceAndKeepsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old", "state.json")
	dst := filepath.Join(dir, "new", "state.json")
	old := &PluginState{
		Config:       Config{RefreshInterval: 9 * time.Hour, TriggerLeadTime: 5 * time.Hour, EnabledAccounts: []string{"acct"}},
		Accounts:     map[string]AccountRuntime{},
		ResetHistory: []ResetRecord{{AuthID: "acct", Success: true, PostWeeklyPct: 100}},
	}
	if err := saveState(src, old); err != nil {
		t.Fatalf("save src: %v", err)
	}

	if !migrateStateFile(src, dst) {
		t.Fatal("first migration reported no-op")
	}
	got, source, err := loadState(dst)
	if err != nil || source != "main" {
		t.Fatalf("load migrated: source=%s err=%v", source, err)
	}
	if len(got.ResetHistory) != 1 || len(got.Config.EnabledAccounts) != 1 {
		t.Fatalf("migration lost data: %+v", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source not moved away: %v", err)
	}
	if _, err := os.Stat(src + ".migrated"); err != nil {
		t.Fatalf("source backup missing: %v", err)
	}
	// Second run: destination exists — must not overwrite it.
	if migrateStateFile(src+".migrated", dst) {
		t.Fatal("second migration must not overwrite the destination")
	}
}

// TestMigrateStateFile_MissingSourceIsNoOp covers the common boot path where
// there is simply nothing to migrate.
func TestMigrateStateFile_MissingSourceIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if migrateStateFile(filepath.Join(dir, "none.json"), filepath.Join(dir, "dst", "state.json")) {
		t.Fatal("missing source must be a no-op")
	}
	if _, err := os.Stat(filepath.Join(dir, "dst")); !os.IsNotExist(err) {
		t.Fatalf("destination should not be created: %v", err)
	}
}

// TestLoadState_CorruptMainRecoversFromBackup verifies the recovery ladder:
// a corrupt main file is quarantined (bytes preserved) and the one-generation
// .bak is loaded instead of silently starting empty.
func TestLoadState_CorruptMainRecoversFromBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	good := &PluginState{
		Config:       DefaultConfig(),
		Accounts:     map[string]AccountRuntime{},
		ResetHistory: []ResetRecord{{AuthID: "a", Success: true}},
	}
	if err := saveState(path, good); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Load once so the .bak exists, then corrupt the main file.
	if _, source, err := loadState(path); err != nil || source != "main" {
		t.Fatalf("prime load: source=%s err=%v", source, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	got, source, err := loadState(path)
	if err != nil {
		t.Fatalf("load with corrupt main: %v", err)
	}
	if !strings.HasPrefix(source, "corrupt-main,backup") {
		t.Fatalf("source = %q, want corrupt-main,backup*", source)
	}
	if len(got.ResetHistory) != 1 || !got.ResetHistory[0].Success {
		t.Fatalf("backup data not recovered: %+v", got.ResetHistory)
	}
	// The corrupt bytes must be preserved for inspection, not destroyed.
	quarantined := false
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "state.json.corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatalf("corrupt file was not quarantined: %v", entries)
	}
}

// TestLoadState_CorruptMainNoBackupStartsFresh verifies that with no usable
// backup, a corrupt main still yields a valid empty state (and the bytes are
// quarantined rather than overwritten in place).
func TestLoadState_CorruptMainNoBackupStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("]]]garbage"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	got, source, err := loadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if source != "corrupt-main,fresh-after-corrupt" {
		t.Fatalf("source = %q", source)
	}
	if got == nil || got.Accounts == nil || len(got.Accounts) != 0 {
		t.Fatalf("want fresh empty state, got %+v", got)
	}
}

// TestLoadState_MissingMainFallsBackToBackup covers main deleted/lost but
// the .bak still present.
func TestLoadState_MissingMainFallsBackToBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveState(path, &PluginState{Config: DefaultConfig(), ResetHistory: []ResetRecord{{AuthID: "b"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, err := loadState(path); err != nil { // prime .bak
		t.Fatalf("prime: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove main: %v", err)
	}
	got, source, err := loadState(path)
	if err != nil || source != "backup" {
		t.Fatalf("load: source=%s err=%v", source, err)
	}
	if len(got.ResetHistory) != 1 {
		t.Fatalf("backup not used: %+v", got.ResetHistory)
	}
}

// TestLoadState_RefreshesBackupAfterGoodLoad verifies the one-generation
// backup is kept current after every successful main-file load.
func TestLoadState_RefreshesBackupAfterGoodLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	first := &PluginState{Config: DefaultConfig(), ResetHistory: []ResetRecord{{AuthID: "one"}}}
	if err := saveState(path, first); err != nil {
		t.Fatalf("save1: %v", err)
	}
	if _, _, err := loadState(path); err != nil {
		t.Fatalf("load1: %v", err)
	}
	second := &PluginState{Config: DefaultConfig(), ResetHistory: []ResetRecord{{AuthID: "one"}, {AuthID: "two"}}}
	if err := saveState(path, second); err != nil {
		t.Fatalf("save2: %v", err)
	}
	if _, _, err := loadState(path); err != nil {
		t.Fatalf("load2: %v", err)
	}
	// Simulate main loss; the .bak must hold the NEWEST generation.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got, source, err := loadState(path)
	if err != nil || source != "backup" {
		t.Fatalf("load: source=%s err=%v", source, err)
	}
	if len(got.ResetHistory) != 2 {
		t.Fatalf("stale backup: %+v", got.ResetHistory)
	}
}
