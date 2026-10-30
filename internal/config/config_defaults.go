package config

import "strings"

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"
	DefaultDiscoveryServiceType  = "_ai-gateway._tcp"

	// DefaultWorkBuddyWorkers is the default WorkBuddy task scheduler worker count.
	DefaultWorkBuddyWorkers = 4
	// DefaultWorkBuddyRequestTimeout is the default timeout for non-chat WorkBuddy task calls.
	DefaultWorkBuddyRequestTimeout = "30s"
	// DefaultWorkBuddyTimezone is the default timezone used for daily task windows.
	DefaultWorkBuddyTimezone = "Asia/Shanghai"
)

// workBuddyTaskNames lists task names in the same order as pointers().
var workBuddyTaskNames = []string{
	"checkin", "balance", "keepalive", "report", "growth",
	"travel", "night-owl", "sweep", "compensation", "streak",
}

// pointers returns addressable references to the configured tasks in order.
func (c *WorkBuddyTasksConfig) pointers() []*WorkBuddyTaskConfig {
	return []*WorkBuddyTaskConfig{
		&c.Checkin, &c.Balance, &c.Keepalive, &c.Report, &c.Growth,
		&c.Travel, &c.NightOwl, &c.Sweep, &c.Compensation, &c.Streak,
	}
}

// Configured reports whether any WorkBuddy setting was provided. An entirely
// zero section is treated as absent so saved config files keep omitting it.
func (c WorkBuddyConfig) Configured() bool {
	if c.Enabled || c.StateDir != "" || c.Workers != 0 || c.RequestTimeout != "" || c.Timezone != "" {
		return true
	}
	for _, task := range c.Tasks.pointers() {
		if task.Enabled != nil || task.Interval != "" || task.DailyAt != "" || task.Timeout != "" {
			return true
		}
	}
	return false
}

// NormalizeWorkBuddyConfig applies WorkBuddy defaults without overwriting
// explicitly configured values. An empty or zero value means "use the default".
// A section with no configured values is left untouched so it stays omitted
// from saved YAML.
func (cfg *Config) NormalizeWorkBuddyConfig() {
	if cfg == nil {
		return
	}
	wb := &cfg.WorkBuddy
	if !wb.Configured() {
		return
	}
	wb.StateDir = strings.TrimSpace(wb.StateDir)
	wb.RequestTimeout = strings.TrimSpace(wb.RequestTimeout)
	if wb.RequestTimeout == "" {
		wb.RequestTimeout = DefaultWorkBuddyRequestTimeout
	}
	wb.Timezone = strings.TrimSpace(wb.Timezone)
	if wb.Timezone == "" {
		wb.Timezone = DefaultWorkBuddyTimezone
	}
	if wb.Workers == 0 {
		wb.Workers = DefaultWorkBuddyWorkers
	}
	for _, task := range wb.Tasks.pointers() {
		task.Interval = strings.TrimSpace(task.Interval)
		task.DailyAt = strings.TrimSpace(task.DailyAt)
		task.Timeout = strings.TrimSpace(task.Timeout)
	}
}
