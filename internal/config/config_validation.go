package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

type namedWorkBuddyTask struct {
	name string
	cfg  WorkBuddyTaskConfig
}

func (c WorkBuddyTasksConfig) named() []namedWorkBuddyTask {
	tasks := c.pointers()
	out := make([]namedWorkBuddyTask, 0, len(tasks))
	for i, task := range tasks {
		name := ""
		if i < len(workBuddyTaskNames) {
			name = workBuddyTaskNames[i]
		}
		out = append(out, namedWorkBuddyTask{name: name, cfg: *task})
	}
	return out
}

// maxWorkBuddyWorkers bounds the scheduler worker pool so a bad configuration
// cannot spawn an unbounded number of goroutines.
const maxWorkBuddyWorkers = 64

// Validate checks the WorkBuddy configuration for invalid scheduling values.
func (c WorkBuddyConfig) Validate() error {
	if c.Workers < 0 {
		return fmt.Errorf("workbuddy.workers must be >= 0, got %d", c.Workers)
	}
	if c.Workers > maxWorkBuddyWorkers {
		return fmt.Errorf("workbuddy.workers must be <= %d, got %d", maxWorkBuddyWorkers, c.Workers)
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("workbuddy.timezone %q is not a valid IANA timezone: %w", c.Timezone, err)
		}
	}
	for _, task := range c.Tasks.named() {
		if task.cfg.Interval != "" {
			if _, err := time.ParseDuration(task.cfg.Interval); err != nil {
				return fmt.Errorf("workbuddy.tasks.%s.interval %q is not a valid duration: %w", task.name, task.cfg.Interval, err)
			}
		}
		if task.cfg.DailyAt != "" {
			if err := validateWorkBuddyDailyAt(task.cfg.DailyAt); err != nil {
				return fmt.Errorf("workbuddy.tasks.%s.daily-at %q: %w", task.name, task.cfg.DailyAt, err)
			}
		}
	}
	return nil
}

// validateWorkBuddyDailyAt accepts an exact HH:MM wall-clock time.
func validateWorkBuddyDailyAt(value string) error {
	if len(value) != 5 || value[2] != ':' {
		return fmt.Errorf("must use HH:MM format")
	}
	if _, err := time.Parse("15:04", value); err != nil {
		return fmt.Errorf("must use HH:MM format: %w", err)
	}
	return nil
}

// SanitizePayloadRules validates raw JSON payload rule params and drops invalid rules.
func (cfg *Config) SanitizePayloadRules() {
	if cfg == nil {
		return
	}
	cfg.Payload.DefaultRaw = sanitizePayloadRawRules(cfg.Payload.DefaultRaw, "default-raw")
	cfg.Payload.OverrideRaw = sanitizePayloadRawRules(cfg.Payload.OverrideRaw, "override-raw")
}

func sanitizePayloadRawRules(rules []PayloadRule, section string) []PayloadRule {
	if len(rules) == 0 {
		return rules
	}
	out := make([]PayloadRule, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if len(rule.Params) == 0 {
			continue
		}
		invalid := false
		for path, value := range rule.Params {
			raw, ok := payloadRawString(value)
			if !ok {
				continue
			}
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || !json.Valid(trimmed) {
				log.WithFields(log.Fields{
					"section":    section,
					"rule_index": i + 1,
					"param":      path,
				}).Warn("payload rule dropped: invalid raw JSON")
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func payloadRawString(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		return nil, false
	}
}

// looksLikeBcrypt returns true if the provided string appears to be a bcrypt hash.
func looksLikeBcrypt(s string) bool {
	return len(s) > 4 && (s[:4] == "$2a$" || s[:4] == "$2b$" || s[:4] == "$2y$")
}

// hashSecret hashes the given secret using bcrypt.
func hashSecret(secret string) (string, error) {
	// Use default cost for simplicity.
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashedBytes), nil
}
