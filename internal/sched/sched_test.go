package sched

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	// A daily 02:00 schedule fires tomorrow morning.
	next := NextRun("0 2 * * *", true, now)
	if next == nil {
		t.Fatal("NextRun for a daily schedule = nil")
	}
	if !next.After(now) {
		t.Fatalf("NextRun = %s, want a time after %s", next, now)
	}
	if next.Sub(now) > 25*time.Hour {
		t.Fatalf("NextRun = %s, more than a day after %s", next, now)
	}
	if next.Location() != time.UTC {
		t.Fatalf("NextRun location = %s, want UTC", next.Location())
	}

	// Every-minute schedules land within the minute.
	if soon := NextRun("* * * * *", true, now); soon == nil || soon.Sub(now) > time.Minute {
		t.Fatalf("NextRun for a per-minute schedule = %v", soon)
	}

	// Null in every case the API contract calls null.
	for _, c := range []struct {
		what     string
		schedule string
		enabled  bool
	}{
		{"manual schedule", ManualSchedule, true},
		{"empty schedule", "", true},
		{"disabled job", "0 2 * * *", false},
		{"unparsable spec", "not a cron", true},
	} {
		if got := NextRun(c.schedule, c.enabled, now); got != nil {
			t.Errorf("NextRun for a %s = %s, want nil", c.what, got)
		}
	}
}
