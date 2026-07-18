package main

import (
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
	if len(cfg.EnabledAccounts) != 0 {
		t.Fatalf("enabled = %v", cfg.EnabledAccounts)
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
	// Empty YAML should yield defaults (normalization kicks in).
	if cfg.RefreshInterval != 12*time.Hour {
		t.Fatalf("empty R should default, got %v", cfg.RefreshInterval)
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
