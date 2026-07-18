package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the 3 user-tunable fields (spec §7.1). All other behavior is
// hardcoded in constants.go — endpoints, retry delays, post-reset delay,
// log limits. Endpoints and OpenAI parameters are NEVER user-configurable.
type Config struct {
	RefreshInterval time.Duration `yaml:"refresh_interval" json:"-"`
	TriggerLeadTime time.Duration `yaml:"trigger_lead_time" json:"-"`
	EnabledAccounts []string      `yaml:"enabled_accounts" json:"enabled_accounts"`
}

// MarshalJSON renders Duration fields as human-readable strings ("12h",
// "6h", "1h30m") instead of Go's default nanosecond integer, which is
// unreadable in the management UI's raw JSON view.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias struct {
		RefreshInterval string   `json:"refresh_interval"`
		TriggerLeadTime string   `json:"trigger_lead_time"`
		EnabledAccounts []string `json:"enabled_accounts"`
	}
	return json.Marshal(alias{
		RefreshInterval: formatDuration(c.RefreshInterval),
		TriggerLeadTime: formatDuration(c.TriggerLeadTime),
		EnabledAccounts: c.EnabledAccounts,
	})
}

// formatDuration trims Go's verbose "12h0m0s" form to the minimal "12h".
// "1h30m0s" -> "1h30m", "90m0s" -> "90m", "45s" stays. Mirrors the scheduler
// plugin's formatDuration so duration displays are consistent across plugins.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// UnmarshalJSON accepts both the string form ("12h") and the raw nanosecond
// integer form (43200000000000) for Duration fields, for round-trip safety.
func (c *Config) UnmarshalJSON(raw []byte) error {
	type alias struct {
		RefreshInterval json.RawMessage `json:"refresh_interval"`
		TriggerLeadTime json.RawMessage `json:"trigger_lead_time"`
		EnabledAccounts []string        `json:"enabled_accounts"`
	}
	var a alias
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	c.EnabledAccounts = a.EnabledAccounts
	d1, err := parseDurationJSON(a.RefreshInterval)
	if err != nil {
		return fmt.Errorf("refresh_interval: %w", err)
	}
	c.RefreshInterval = d1
	d2, err := parseDurationJSON(a.TriggerLeadTime)
	if err != nil {
		return fmt.Errorf("trigger_lead_time: %w", err)
	}
	c.TriggerLeadTime = d2
	return nil
}

// parseDurationJSON accepts a JSON string ("12h") or a JSON number (nanoseconds).
func parseDurationJSON(raw json.RawMessage) (time.Duration, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, nil
	}
	// String form: "12h", "1h30m", etc.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("parse %q: %w", s, err)
		}
		return d, nil
	}
	// Number form: nanoseconds (Go default).
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return time.Duration(n), nil
	}
	return 0, fmt.Errorf("must be a duration string like \"12h\" or a nanosecond integer")
}

// DefaultConfig returns the spec defaults: 12h patrol, 6h lead time, no
// accounts enabled (empty == soft-disable until the user opts accounts in).
func DefaultConfig() Config {
	return Config{
		RefreshInterval: 12 * time.Hour,
		TriggerLeadTime: 6 * time.Hour,
		EnabledAccounts: nil,
	}
}

// parseConfigYAML parses user YAML and normalizes it. Malformed YAML returns
// an error; missing fields fall back to defaults via normalizeConfig.
func parseConfigYAML(raw []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config yaml: %w", err)
	}
	return normalizeConfig(cfg), nil
}

// normalizeConfig applies floors and de-dup. Intervals below 1 minute are
// raised to 1 minute to prevent tight worker loops. Empty/duplicate account
// IDs are dropped.
func normalizeConfig(cfg Config) Config {
	if cfg.RefreshInterval < time.Minute {
		cfg.RefreshInterval = 12 * time.Hour
	}
	if cfg.TriggerLeadTime < time.Minute {
		cfg.TriggerLeadTime = 6 * time.Hour
	}
	out := make([]string, 0, len(cfg.EnabledAccounts))
	seen := map[string]bool{}
	for _, a := range cfg.EnabledAccounts {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	cfg.EnabledAccounts = out
	return cfg
}
