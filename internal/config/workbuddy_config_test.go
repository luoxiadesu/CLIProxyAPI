package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkBuddyConfigDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("workbuddy:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if !cfg.WorkBuddy.Enabled {
		t.Fatal("workbuddy.enabled should be true")
	}
	if cfg.WorkBuddy.Workers != DefaultWorkBuddyWorkers {
		t.Fatalf("workers = %d, want %d", cfg.WorkBuddy.Workers, DefaultWorkBuddyWorkers)
	}
	if cfg.WorkBuddy.RequestTimeout != DefaultWorkBuddyRequestTimeout {
		t.Fatalf("request-timeout = %q, want %q", cfg.WorkBuddy.RequestTimeout, DefaultWorkBuddyRequestTimeout)
	}
	if cfg.WorkBuddy.Timezone != DefaultWorkBuddyTimezone {
		t.Fatalf("timezone = %q, want %q", cfg.WorkBuddy.Timezone, DefaultWorkBuddyTimezone)
	}
	for name, task := range map[string]WorkBuddyTaskConfig{
		"checkin":      cfg.WorkBuddy.Tasks.Checkin,
		"balance":      cfg.WorkBuddy.Tasks.Balance,
		"keepalive":    cfg.WorkBuddy.Tasks.Keepalive,
		"report":       cfg.WorkBuddy.Tasks.Report,
		"growth":       cfg.WorkBuddy.Tasks.Growth,
		"travel":       cfg.WorkBuddy.Tasks.Travel,
		"night-owl":    cfg.WorkBuddy.Tasks.NightOwl,
		"sweep":        cfg.WorkBuddy.Tasks.Sweep,
		"compensation": cfg.WorkBuddy.Tasks.Compensation,
		"streak":       cfg.WorkBuddy.Tasks.Streak,
	} {
		if !task.IsEnabled() {
			t.Fatalf("task %s should default to enabled", name)
		}
	}
}

func TestWorkBuddyConfigPreservesExplicitValues(t *testing.T) {
	raw := `
workbuddy:
  enabled: true
  state-dir: "/tmp/wb-state"
  workers: 2
  request-timeout: "5s"
  timezone: "UTC"
  tasks:
    checkin:
      enabled: false
      daily-at: "08:00"
    balance:
      interval: "6h"
      timeout: "10s"
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.WorkBuddy.Workers != 2 || cfg.WorkBuddy.RequestTimeout != "5s" || cfg.WorkBuddy.Timezone != "UTC" {
		t.Fatalf("explicit workbuddy values were overwritten: %#v", cfg.WorkBuddy)
	}
	if cfg.WorkBuddy.StateDir != "/tmp/wb-state" {
		t.Fatalf("state-dir = %q", cfg.WorkBuddy.StateDir)
	}
	if cfg.WorkBuddy.Tasks.Checkin.IsEnabled() {
		t.Fatal("checkin should be explicitly disabled")
	}
	if cfg.WorkBuddy.Tasks.Checkin.DailyAt != "08:00" {
		t.Fatalf("checkin daily-at = %q", cfg.WorkBuddy.Tasks.Checkin.DailyAt)
	}
	if !cfg.WorkBuddy.Tasks.Balance.IsEnabled() || cfg.WorkBuddy.Tasks.Balance.Interval != "6h" {
		t.Fatalf("balance config = %#v", cfg.WorkBuddy.Tasks.Balance)
	}
	if cfg.WorkBuddy.Tasks.Balance.Timeout != "10s" {
		t.Fatalf("balance timeout config = %#v", cfg.WorkBuddy.Tasks.Balance)
	}
}

func TestWorkBuddyConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"negative workers", "workbuddy:\n  workers: -1\n"},
		{"bad interval", "workbuddy:\n  tasks:\n    balance:\n      interval: \"not-a-duration\"\n"},
		{"bad daily-at hour", "workbuddy:\n  tasks:\n    checkin:\n      daily-at: \"25:00\"\n"},
		{"bad daily-at shape", "workbuddy:\n  tasks:\n    checkin:\n      daily-at: \"8:00\"\n"},
		{"bad timezone", "workbuddy:\n  timezone: \"Not/AZone\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfigBytes([]byte(tc.raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestWorkBuddyConfigValidationAcceptsValidValues(t *testing.T) {
	raw := `
workbuddy:
  workers: 0
  timezone: "Asia/Shanghai"
  tasks:
    checkin: {daily-at: "00:00"}
    balance: {interval: "1h30m"}
    night-owl: {daily-at: "23:59"}
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.WorkBuddy.Workers != DefaultWorkBuddyWorkers {
		t.Fatalf("workers = %d, want default %d", cfg.WorkBuddy.Workers, DefaultWorkBuddyWorkers)
	}
}

func TestWorkBuddyV8Layout(t *testing.T) {
	raw := []byte(`
config-version: 8
workbuddy:
  enabled: true
  workers: 2
  tasks:
    checkin: {enabled: true, daily-at: "08:00"}
    balance: {interval: "6h"}
`)
	if err := ValidateV8Config(raw); err != nil {
		t.Fatalf("v8 validation: %v", err)
	}
	normalized, changed, err := NormalizeConfigLayout(raw, false)
	if err != nil {
		t.Fatalf("normalize layout: %v", err)
	}
	if changed {
		t.Fatalf("workbuddy section should not be migrated: %s", normalized)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !IsV8ConfigLayout(doc.Content[0]) {
		t.Fatal("workbuddy should mark the document as v8")
	}
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.WorkBuddy.Workers != 2 || !cfg.WorkBuddy.Tasks.Checkin.IsEnabled() {
		t.Fatalf("v8 workbuddy config = %#v", cfg.WorkBuddy)
	}
}

func TestWorkBuddyV8RejectsUnknownTaskField(t *testing.T) {
	raw := []byte("config-version: 8\nworkbuddy:\n  tasks:\n    checkin:\n      unknown-field: true\n")
	if err := ValidateV8Config(raw); err == nil {
		t.Fatal("expected unknown workbuddy task field to be rejected")
	}
}

// TestWorkBuddyConfigValidationOnFileLoad proves the validation hook is reached
// through the LoadConfig / LoadConfigOptional path, not only ParseConfigBytes.
func TestWorkBuddyConfigValidationOnFileLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("workbuddy:\n  tasks:\n    balance:\n      interval: \"not-a-duration\"\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected workbuddy validation error from LoadConfig")
	}
}

func TestWorkBuddyConfigDefaultsOnFileLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("workbuddy:\n  enabled: true\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.WorkBuddy.Workers != DefaultWorkBuddyWorkers ||
		cfg.WorkBuddy.RequestTimeout != DefaultWorkBuddyRequestTimeout ||
		cfg.WorkBuddy.Timezone != DefaultWorkBuddyTimezone {
		t.Fatalf("file load did not apply workbuddy defaults: %#v", cfg.WorkBuddy)
	}
}

func TestWorkBuddyOmittedWhenUnconfigured(t *testing.T) {
	for _, raw := range []string{
		"port: 8317\nauth-dir: \"./auths\"\n",
		"workbuddy: {}\n",
	} {
		cfg, err := ParseConfigBytes([]byte(raw))
		if err != nil {
			t.Fatalf("parse config %q: %v", raw, err)
		}
		if cfg.WorkBuddy.Configured() {
			t.Fatalf("unconfigured workbuddy reported Configured: %#v", cfg.WorkBuddy)
		}
		out, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		if strings.Contains(string(out), "workbuddy") {
			t.Fatalf("unconfigured workbuddy leaked into YAML:\n%s", out)
		}
	}
}

func TestWorkBuddyEmittedWhenConfigured(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("workbuddy:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if !strings.Contains(string(out), "workbuddy:") {
		t.Fatalf("configured workbuddy missing from YAML:\n%s", out)
	}
}

// TestWorkBuddyNotPersistedForLegacySave covers the real regression: saving an
// unrelated legacy config must not introduce a workbuddy root, which would make
// the next load treat the document as v8.
func TestWorkBuddyNotPersistedForLegacySave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nauth-dir: \"./auths\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "workbuddy") {
		t.Fatalf("legacy save introduced workbuddy:\n%s", data)
	}
}
