package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RefreshInterval != 12*time.Hour {
		t.Fatalf("R = %v", cfg.RefreshInterval)
	}
	if cfg.TriggerLeadTime != 6*time.Hour {
		t.Fatalf("L = %v", cfg.TriggerLeadTime)
	}
	// Fresh installs enable every Codex account via the wildcard.
	if len(cfg.EnabledAccounts) != 1 || cfg.EnabledAccounts[0] != allAccountsWildcard {
		t.Fatalf("enabled = %v, want [%q]", cfg.EnabledAccounts, allAccountsWildcard)
	}
}

func TestParseConfigYAML(t *testing.T) {
	raw := []byte(`
refresh_interval: 4h
trigger_lead_time: 2h
enabled_accounts: [a, b]
`)
	cfg, err := parseConfigYAML(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.RefreshInterval != 4*time.Hour || cfg.TriggerLeadTime != 2*time.Hour {
		t.Fatalf("parsed = %+v", cfg)
	}
	if len(cfg.EnabledAccounts) != 2 || cfg.EnabledAccounts[0] != "a" {
		t.Fatalf("enabled = %v", cfg.EnabledAccounts)
	}
}

func TestParseConfigYAML_Empty(t *testing.T) {
	cfg, err := parseConfigYAML([]byte(``))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	// Empty YAML should yield defaults (normalization kicks in), including
	// the all-accounts wildcard for a fresh install.
	if cfg.RefreshInterval != 12*time.Hour {
		t.Fatalf("empty R should default, got %v", cfg.RefreshInterval)
	}
	if !configEnablesAll(cfg.EnabledAccounts) {
		t.Fatalf("empty yaml enabled = %v, want wildcard", cfg.EnabledAccounts)
	}
}

// TestParseConfigYAML_ExplicitEmptyOptsOut verifies `enabled_accounts: []`
// is honored as a deliberate opt-out rather than inheriting the wildcard
// default (which only applies when the field is absent).
func TestParseConfigYAML_ExplicitEmptyOptsOut(t *testing.T) {
	cfg, err := parseConfigYAML([]byte("enabled_accounts: []\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.EnabledAccounts == nil || len(cfg.EnabledAccounts) != 0 {
		t.Fatalf("enabled = %#v, want explicit empty slice", cfg.EnabledAccounts)
	}
}

// TestNormalizeConfig_WildcardCollapsesExplicitEntries: "*" already includes
// every account, so mixing it with explicit indexes collapses to ["*"].
func TestNormalizeConfig_WildcardCollapsesExplicitEntries(t *testing.T) {
	cfg := Config{
		RefreshInterval: 12 * time.Hour,
		TriggerLeadTime: 6 * time.Hour,
		EnabledAccounts: []string{"*", "  ", "a", "*"},
	}
	cfg = normalizeConfig(cfg)
	if len(cfg.EnabledAccounts) != 1 || cfg.EnabledAccounts[0] != allAccountsWildcard {
		t.Fatalf("enabled = %v, want [%q]", cfg.EnabledAccounts, allAccountsWildcard)
	}
}

func TestNormalizeConfig_FloorsIntervals(t *testing.T) {
	cfg := Config{RefreshInterval: 1 * time.Second, TriggerLeadTime: 0, EnabledAccounts: []string{"x"}}
	cfg = normalizeConfig(cfg)
	// Floor intervals to 1 minute to avoid tight loops.
	if cfg.RefreshInterval < time.Minute {
		t.Fatalf("R not floored: %v", cfg.RefreshInterval)
	}
	if cfg.TriggerLeadTime < time.Minute {
		t.Fatalf("L not floored: %v", cfg.TriggerLeadTime)
	}
}

func TestNormalizeConfig_DedupAndTrimAccounts(t *testing.T) {
	cfg := Config{
		RefreshInterval: 12 * time.Hour,
		TriggerLeadTime: 6 * time.Hour,
		EnabledAccounts: []string{"  a  ", "b", "a", "", "c"},
	}
	cfg = normalizeConfig(cfg)
	want := []string{"a", "b", "c"}
	if len(cfg.EnabledAccounts) != len(want) {
		t.Fatalf("enabled = %v, want %v", cfg.EnabledAccounts, want)
	}
	for i, w := range want {
		if cfg.EnabledAccounts[i] != w {
			t.Fatalf("enabled[%d] = %q, want %q (full: %v)", i, cfg.EnabledAccounts[i], w, cfg.EnabledAccounts)
		}
	}
}

func TestParseConfigYAML_Malformed(t *testing.T) {
	_, err := parseConfigYAML([]byte(`not: [valid: yaml`))
	if err == nil {
		t.Fatalf("expected error on malformed YAML")
	}
}

// TestConfig_MarshalJSON_ProducesDurationStrings verifies the management UI's
// status JSON shows "12h" instead of the raw nanosecond integer
// (43200000000000). Regression guard for the unreadable-JSON bug.
func TestConfig_MarshalJSON_ProducesDurationStrings(t *testing.T) {
	cfg := Config{RefreshInterval: 12 * time.Hour, TriggerLeadTime: 6 * time.Hour, EnabledAccounts: []string{"a"}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"refresh_interval":"12h"`, `"trigger_lead_time":"6h"`, `"enabled_accounts":["a"]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("JSON missing %q; got %s", want, body)
		}
	}
	if strings.Contains(body, "43200000000000") {
		t.Fatalf("JSON leaked nanosecond integer: %s", body)
	}
}

// TestConfig_UnmarshalJSON_AcceptsStringAndNanoseconds verifies round-trip
// from both the human string form and Go's default nanosecond integer form.
func TestConfig_UnmarshalJSON_AcceptsStringAndNanoseconds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"string_form", `{"refresh_interval":"4h"}`, 4 * time.Hour},
		{"ns_integer_form", `{"refresh_interval":14400000000000}`, 4 * time.Hour},
		{"empty", `{"refresh_interval":""}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if c.RefreshInterval != tc.want {
				t.Fatalf("RefreshInterval = %v, want %v", c.RefreshInterval, tc.want)
			}
		})
	}
}

// TestParseConfigYAML_CpaEnableBlockOnlyDefaultsToWildcard: a fresh install's
// config_yaml is just CPA's enable block (`enabled: true`, maybe `priority`);
// enabled_accounts is absent and must inherit the all-accounts wildcard so
// the plugin works immediately with zero UI setup.
func TestParseConfigYAML_CpaEnableBlockOnlyDefaultsToWildcard(t *testing.T) {
	cfg, err := parseConfigYAML([]byte("enabled: true\npriority: 1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !configEnablesAll(cfg.EnabledAccounts) {
		t.Fatalf("enabled = %v, want wildcard default", cfg.EnabledAccounts)
	}
}
