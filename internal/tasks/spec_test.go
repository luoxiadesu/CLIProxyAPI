package tasks

import (
	"testing"
	"time"
)

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return location
}

func TestNextDue(t *testing.T) {
	shanghai := mustLocation(t, "Asia/Shanghai")
	now := time.Date(2026, 10, 30, 7, 0, 0, 0, shanghai)
	todayAt := func(hour, minute int) time.Time {
		return time.Date(2026, 10, 30, hour, minute, 0, 0, shanghai)
	}

	tests := []struct {
		name string
		now  time.Time
		spec Spec
		loc  *time.Location
		last time.Time
		want time.Time
		ok   bool
	}{
		{
			name: "disabled",
			now:  now,
			spec: Spec{Enabled: false, Interval: time.Hour},
			loc:  shanghai,
		},
		{
			name: "no schedule",
			now:  now,
			spec: Spec{Enabled: true},
			loc:  shanghai,
		},
		{
			name: "interval first run is immediate",
			now:  now,
			spec: Spec{Enabled: true, Interval: time.Hour},
			loc:  shanghai,
			want: now,
			ok:   true,
		},
		{
			name: "interval waits for cadence",
			now:  now,
			spec: Spec{Enabled: true, Interval: time.Hour},
			loc:  shanghai,
			last: now.Add(-30 * time.Minute),
			want: now.Add(30 * time.Minute),
			ok:   true,
		},
		{
			name: "interval overdue runs now",
			now:  now,
			spec: Spec{Enabled: true, Interval: time.Hour},
			loc:  shanghai,
			last: now.Add(-2 * time.Hour),
			want: now,
			ok:   true,
		},
		{
			name: "daily before window waits for today",
			now:  now,
			spec: Spec{Enabled: true, DailyAt: "08:00"},
			loc:  shanghai,
			want: todayAt(8, 0),
			ok:   true,
		},
		{
			name: "daily after window is due today",
			now:  now.Add(2 * time.Hour),
			spec: Spec{Enabled: true, DailyAt: "08:00"},
			loc:  shanghai,
			want: todayAt(8, 0),
			ok:   true,
		},
		{
			name: "daily after last run schedules tomorrow",
			now:  now.Add(2 * time.Hour),
			spec: Spec{Enabled: true, DailyAt: "08:00"},
			loc:  shanghai,
			last: todayAt(8, 5),
			want: todayAt(8, 0).AddDate(0, 0, 1),
			ok:   true,
		},
		{
			name: "daily with last run yesterday is due today",
			now:  now.Add(2 * time.Hour),
			spec: Spec{Enabled: true, DailyAt: "08:00"},
			loc:  shanghai,
			last: todayAt(8, 5).AddDate(0, 0, -1),
			want: todayAt(8, 0),
			ok:   true,
		},
		{
			name: "daily evaluates the configured timezone",
			now:  time.Date(2026, 10, 30, 23, 30, 0, 0, time.UTC),
			spec: Spec{Enabled: true, DailyAt: "08:00"},
			loc:  shanghai,
			want: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "invalid daily at",
			now:  now,
			spec: Spec{Enabled: true, DailyAt: "8:00"},
			loc:  shanghai,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NextDue(tt.now, tt.spec, tt.loc, tt.last)
			if ok != tt.ok {
				t.Fatalf("NextDue ok = %v, want %v", ok, tt.ok)
			}
			if ok && !got.Equal(tt.want) {
				t.Fatalf("NextDue = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseDailyAt(t *testing.T) {
	tests := []struct {
		value      string
		hour       int
		minute     int
		shouldPass bool
	}{
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{" 08:05 ", 8, 5, true},
		{"24:00", 0, 0, false},
		{"08:60", 0, 0, false},
		{"8:00", 0, 0, false},
		{"08-00", 0, 0, false},
		{"ab:cd", 0, 0, false},
	}
	for _, tt := range tests {
		hour, minute, ok := parseDailyAt(tt.value)
		if ok != tt.shouldPass {
			t.Fatalf("parseDailyAt(%q) ok = %v, want %v", tt.value, ok, tt.shouldPass)
		}
		if ok && (hour != tt.hour || minute != tt.minute) {
			t.Fatalf("parseDailyAt(%q) = %02d:%02d, want %02d:%02d", tt.value, hour, minute, tt.hour, tt.minute)
		}
	}
}

func TestNextTaskRun(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		spec  Spec
		state TaskState
		want  time.Time
		ok    bool
	}{
		{
			name: "short backoff beats long interval",
			spec: Spec{Enabled: true, Interval: time.Hour},
			state: TaskState{
				LastRun:             now.Add(-30 * time.Minute),
				ConsecutiveFailures: 1,
				NextRun:             now.Add(time.Minute),
			},
			want: now.Add(time.Minute),
			ok:   true,
		},
		{
			name: "regular cadence beats longer backoff",
			spec: Spec{Enabled: true, Interval: 30 * time.Second},
			state: TaskState{
				LastRun:             now.Add(-20 * time.Second),
				ConsecutiveFailures: 1,
				NextRun:             now.Add(5 * time.Minute),
			},
			want: now.Add(10 * time.Second),
			ok:   true,
		},
		{
			name: "success ignores the retry instant",
			spec: Spec{Enabled: true, Interval: time.Hour},
			state: TaskState{
				LastRun: now.Add(-30 * time.Minute),
				NextRun: now.Add(time.Minute),
			},
			want: now.Add(30 * time.Minute),
			ok:   true,
		},
		{
			name:  "disabled",
			spec:  Spec{Enabled: false, Interval: time.Hour},
			state: TaskState{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nextTaskRun(now, tt.spec, time.UTC, tt.state)
			if ok != tt.ok {
				t.Fatalf("nextTaskRun ok = %v, want %v", ok, tt.ok)
			}
			if ok && !got.Equal(tt.want) {
				t.Fatalf("nextTaskRun = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, time.Minute},
		{2, 5 * time.Minute},
		{3, 15 * time.Minute},
		{4, time.Hour},
		{5, 2 * time.Hour},
		{6, 4 * time.Hour},
		{7, maxBackoff},
		{20, maxBackoff},
	}
	for _, tt := range tests {
		if got := backoffDelay(tt.failures); got != tt.want {
			t.Fatalf("backoffDelay(%d) = %s, want %s", tt.failures, got, tt.want)
		}
	}
}
