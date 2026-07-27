package sched

import (
	"testing"
	"time"

	"proxback/internal/store"
)

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	// A daily 02:00 schedule fires within the next day, whatever the server's
	// zone is — the schedule means 02:00 local time.
	next := NextRun(store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}, true, now)
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
	if local := next.In(time.Local); local.Hour() != 2 || local.Minute() != 0 {
		t.Fatalf("NextRun = %s local, want 02:00 in the server's zone", local)
	}

	// Every-minute schedules land within the minute.
	soon := NextRun(store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "* * * * *"}, true, now)
	if soon == nil || soon.Sub(now) > time.Minute {
		t.Fatalf("NextRun for a per-minute schedule = %v", soon)
	}

	// Null in every case the API contract calls null.
	for _, c := range []struct {
		what     string
		schedule store.Schedule
		enabled  bool
	}{
		{"manual schedule", store.ManualSchedule(), true},
		{"empty schedule", store.Schedule{}, true},
		{"disabled job", store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}, false},
		{"unparsable spec", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "not a cron"}, true},
	} {
		if got := NextRun(c.schedule, c.enabled, now); got != nil {
			t.Errorf("NextRun for a %s = %s, want nil", c.what, got)
		}
	}
}

// TestNextRunSkipsNonLastDaysOfMonth is the scheduler half of the last-day-of-
// month schedule: its cron expression covers days 28–31, so NextRun has to walk
// past the firings that are not actually the month's last day. A 28, a 29, a 30
// and a 31 day month between them cover every case there is.
func TestNextRunSkipsNonLastDaysOfMonth(t *testing.T) {
	lastDay := store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: store.LastDayOfMonth}
	for _, c := range []struct {
		name string
		from time.Time
		want time.Time
	}{
		{"28 day February", time.Date(2026, 2, 1, 9, 0, 0, 0, time.Local), time.Date(2026, 2, 28, 1, 0, 0, 0, time.Local)},
		{"29 day February", time.Date(2028, 2, 1, 9, 0, 0, 0, time.Local), time.Date(2028, 2, 29, 1, 0, 0, 0, time.Local)},
		{"30 day April", time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local), time.Date(2026, 4, 30, 1, 0, 0, 0, time.Local)},
		{"31 day January", time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local), time.Date(2026, 1, 31, 1, 0, 0, 0, time.Local)},
		// Standing on the 28th of a 31 day month, the next firing is the 31st,
		// not that night: the 28th is not the last day here.
		{"late in a 31 day month", time.Date(2026, 1, 28, 9, 0, 0, 0, time.Local), time.Date(2026, 1, 31, 1, 0, 0, 0, time.Local)},
	} {
		got := NextRun(lastDay, true, c.from)
		if got == nil {
			t.Fatalf("%s: NextRun = nil", c.name)
		}
		if !got.Equal(c.want) {
			t.Errorf("%s: NextRun = %s, want %s", c.name, got.In(time.Local), c.want)
		}
	}
}
