package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// PluginState is the on-disk persistent shape. The plugin writes this to
// <state-dir>/codex-auto-reset.state.json after every meaningful change so a
// restart resumes patrols and survives crashes without losing account state.
type PluginState struct {
	Config   Config                    `json:"config"`
	Accounts map[string]AccountRuntime `json:"accounts"`
}

// AccountRuntime is the persisted per-account FSM snapshot. Only the fields
// needed to resume scheduling are stored; transient step state is not.
type AccountRuntime struct {
	AuthID   string       `json:"auth_id"`
	State    State        `json:"state"`
	NextWake time.Time    `json:"next_wake"`
	Attempt  ResetAttempt `json:"attempt,omitempty"`
}

// saveState writes state atomically: marshal → write to <path>.tmp → rename.
// The tmp+rename pattern ensures a crash mid-write leaves the prior file
// intact rather than a truncated one.
func saveState(path string, s *PluginState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadState reads state. A missing file returns an empty (but non-nil) state
// rather than an error — first-run behavior.
func loadState(path string) (*PluginState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &PluginState{Accounts: map[string]AccountRuntime{}}, nil
		}
		return nil, err
	}
	var s PluginState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Accounts == nil {
		s.Accounts = map[string]AccountRuntime{}
	}
	return &s, nil
}
