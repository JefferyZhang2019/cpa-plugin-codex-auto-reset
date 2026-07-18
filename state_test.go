package main

import (
	"os"
	"path/filepath"
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
