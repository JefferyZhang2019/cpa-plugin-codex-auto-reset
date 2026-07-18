package main

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the 3 user-tunable fields (spec §7.1). All other behavior is
// hardcoded in constants.go — endpoints, retry delays, post-reset delay,
// log limits. Endpoints and OpenAI parameters are NEVER user-configurable.
type Config struct {
	RefreshInterval time.Duration `yaml:"refresh_interval" json:"refresh_interval"`
	TriggerLeadTime time.Duration `yaml:"trigger_lead_time" json:"trigger_lead_time"`
	EnabledAccounts []string      `yaml:"enabled_accounts" json:"enabled_accounts"`
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
