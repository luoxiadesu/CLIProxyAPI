package tasks

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNewAllDefaultSpecs(t *testing.T) {
	list := NewAll(&config.Config{})
	if len(list) != 10 {
		t.Fatalf("task count = %d, want 10", len(list))
	}
	specs := make(map[string]struct {
		enabled  bool
		interval time.Duration
		dailyAt  string
	})
	for _, task := range list {
		spec := task.Spec()
		specs[spec.Name] = struct {
			enabled  bool
			interval time.Duration
			dailyAt  string
		}{spec.Enabled, spec.Interval, spec.DailyAt}
	}
	cases := []struct {
		name     string
		interval time.Duration
		dailyAt  string
	}{
		{"checkin", 0, defaultCheckinDailyAt},
		{"balance", defaultBalanceInterval, ""},
		{"keepalive", defaultKeepaliveInterval, ""},
		{"report", defaultReportInterval, ""},
		{"growth", defaultGrowthInterval, ""},
		{"travel", defaultTravelInterval, ""},
		{"night-owl", 0, defaultNightOwlDailyAt},
		{"sweep", 0, defaultSweepDailyAt},
		{"compensation", 0, defaultCompensationDailyAt},
		{"streak", 0, defaultStreakDailyAt},
	}
	for _, testCase := range cases {
		spec, ok := specs[testCase.name]
		if !ok {
			t.Fatalf("missing task %s", testCase.name)
		}
		if !spec.enabled {
			t.Fatalf("%s must be enabled by default", testCase.name)
		}
		if spec.interval != testCase.interval {
			t.Fatalf("%s interval = %s, want %s", testCase.name, spec.interval, testCase.interval)
		}
		if spec.dailyAt != testCase.dailyAt {
			t.Fatalf("%s daily-at = %q, want %q", testCase.name, spec.dailyAt, testCase.dailyAt)
		}
	}
}

func TestNewAllConfigOverrides(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		WorkBuddy: config.WorkBuddyConfig{
			Tasks: config.WorkBuddyTasksConfig{
				Checkin: config.WorkBuddyTaskConfig{Enabled: &disabled, DailyAt: "09:30", Timeout: "45s"},
				Balance: config.WorkBuddyTaskConfig{Interval: "2h"},
			},
		},
	}
	list := NewAll(cfg)
	for _, task := range list {
		spec := task.Spec()
		switch spec.Name {
		case "checkin":
			if spec.Enabled {
				t.Fatal("checkin must be disabled")
			}
			if spec.DailyAt != "09:30" {
				t.Fatalf("checkin daily-at = %q", spec.DailyAt)
			}
			if spec.Timeout != 45*time.Second {
				t.Fatalf("checkin timeout = %s", spec.Timeout)
			}
		case "balance":
			if spec.Interval != 2*time.Hour {
				t.Fatalf("balance interval = %s", spec.Interval)
			}
		}
	}
}

func TestNewAllNightOwlTimeout(t *testing.T) {
	list := NewAll(&config.Config{})
	for _, task := range list {
		if task.Spec().Name == "night-owl" && task.Spec().Timeout != defaultNightOwlTimeout {
			t.Fatalf("night-owl timeout = %s, want %s", task.Spec().Timeout, defaultNightOwlTimeout)
		}
	}
}
