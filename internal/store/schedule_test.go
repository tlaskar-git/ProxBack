package store_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"proxback/internal/store"
)

// TestScheduleValidation is the guard between the UI's schedule picker and the
// scheduler: every shape the contract allows is accepted, and everything else is
// refused with a message the operator can act on.
func TestScheduleValidation(t *testing.T) {
	for _, ok := range []store.Schedule{
		store.ManualSchedule(),
		{},                                      // the zero value is a manual schedule
		{Kind: store.ScheduleHourly, Minute: 0}, // the boundaries of every range
		{Kind: store.ScheduleHourly, Minute: 59},
		{Kind: store.ScheduleDaily, Time: "00:00"},
		{Kind: store.ScheduleDaily, Time: "23:59"},
		{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0}},
		{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0, 6}},
		{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{1, 2, 3, 4, 5}},
		{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 1},
		{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 31},
		{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"},
		{Kind: store.ScheduleAdvanced, CronExpr: "@daily"},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", ok, err)
		}
	}

	for _, bad := range []struct {
		what     string
		schedule store.Schedule
		mentions string
	}{
		{"unknown kind", store.Schedule{Kind: "yearly"}, "kind"},
		{"minute above the hour", store.Schedule{Kind: store.ScheduleHourly, Minute: 60}, "minute"},
		{"negative minute", store.Schedule{Kind: store.ScheduleHourly, Minute: -1}, "minute"},
		{"missing time", store.Schedule{Kind: store.ScheduleDaily}, "time"},
		{"24 hour clock rolled over", store.Schedule{Kind: store.ScheduleDaily, Time: "24:00"}, "time"},
		{"minutes past the hour", store.Schedule{Kind: store.ScheduleDaily, Time: "02:60"}, "time"},
		{"unpadded time", store.Schedule{Kind: store.ScheduleDaily, Time: "2:00"}, "time"},
		{"12 hour time", store.Schedule{Kind: store.ScheduleDaily, Time: "2:00pm"}, "time"},
		{"no weekdays", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00"}, "weekday"},
		{"empty weekdays", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{}}, "weekday"},
		{"weekday 7", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{7}}, "weekdays"},
		{"negative weekday", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0, -1}}, "weekdays"},
		{"day zero", store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 0}, "dayOfMonth"},
		{"day 32", store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 32}, "dayOfMonth"},
		{"advanced without a cron", store.Schedule{Kind: store.ScheduleAdvanced}, "cron"},
		{"advanced with a bad cron", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "not a cron"}, "cron"},
		{"advanced with too few fields", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "* * *"}, "cron"},
	} {
		err := bad.schedule.Validate()
		if err == nil {
			t.Errorf("Validate(%s) accepted %+v", bad.what, bad.schedule)
			continue
		}
		if !strings.Contains(err.Error(), bad.mentions) {
			t.Errorf("Validate(%s) = %q, want it to mention %q", bad.what, err, bad.mentions)
		}
	}
}

// TestScheduleCronAndLabel pins the two derivations the rest of the system
// depends on: the expression the scheduler runs on and the English summary the
// UI shows verbatim.
func TestScheduleCronAndLabel(t *testing.T) {
	for _, c := range []struct {
		schedule store.Schedule
		cron     string
		label    string
	}{
		{store.ManualSchedule(), "", "Manual"},
		{store.Schedule{}, "", "Manual"},
		{store.Schedule{Kind: store.ScheduleHourly, Minute: 30}, "30 * * * *", "Every hour at :30"},
		{store.Schedule{Kind: store.ScheduleHourly, Minute: 0}, "0 * * * *", "Every hour at :00"},
		{store.Schedule{Kind: store.ScheduleHourly, Minute: 5}, "5 * * * *", "Every hour at :05"},
		{store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}, "0 2 * * *", "Daily at 02:00"},
		{store.Schedule{Kind: store.ScheduleDaily, Time: "23:45"}, "45 23 * * *", "Daily at 23:45"},
		{store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0, 6}},
			"0 3 * * 0,6", "Weekly on Sun, Sat at 03:00"},
		// Weekdays are de-duplicated and ordered on the way through, so the
		// expression and the label do not depend on the order they were sent in.
		{store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{6, 1, 6}},
			"0 3 * * 1,6", "Weekly on Mon, Sat at 03:00"},
		{store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 1},
			"0 1 1 * *", "Monthly on day 1 at 01:00"},
		{store.Schedule{Kind: store.ScheduleMonthly, Time: "04:30", DayOfMonth: 15},
			"30 4 15 * *", "Monthly on day 15 at 04:30"},
		{store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 31},
			"0 1 28-31 * *", "Monthly on the last day at 01:00"},
		{store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"},
			"*/15 * * * *", "Custom (*/15 * * * *)"},
	} {
		if got := c.schedule.Cron(); got != c.cron {
			t.Errorf("Cron(%+v) = %q, want %q", c.schedule, got, c.cron)
		}
		if got := c.schedule.Label(); got != c.label {
			t.Errorf("Label(%+v) = %q, want %q", c.schedule, got, c.label)
		}
	}
}

// TestScheduleLastDayOfMonth proves the one thing cron cannot express. The
// expression fires on days 28–31; ShouldRun has to keep exactly one firing per
// month, on the day that really is the last one — in a 28, a 29, a 30 and a 31
// day month alike.
func TestScheduleLastDayOfMonth(t *testing.T) {
	lastDay := store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: store.LastDayOfMonth}
	if got := lastDay.Cron(); got != "0 1 28-31 * *" {
		t.Fatalf("last-day cron = %q", got)
	}

	for _, month := range []struct {
		name    string
		year    int
		month   time.Month
		lastDay int
	}{
		{"February 2026", 2026, time.February, 28},
		{"February 2028 (leap)", 2028, time.February, 29},
		{"April 2026", 2026, time.April, 30},
		{"January 2026", 2026, time.January, 31},
	} {
		fired := 0
		// Walk the whole window the expression covers.
		for day := 28; day <= 31; day++ {
			at := time.Date(month.year, month.month, day, 1, 0, 0, 0, time.UTC)
			// A day past the end of the month rolls into the next one; those
			// dates are not firings of this month at all.
			if at.Month() != month.month {
				continue
			}
			want := day == month.lastDay
			if got := lastDay.ShouldRun(at); got != want {
				t.Errorf("%s: ShouldRun(day %d) = %v, want %v", month.name, day, got, want)
			}
			if want {
				fired++
			}
		}
		if fired != 1 {
			t.Errorf("%s: the schedule fired %d times, want exactly 1", month.name, fired)
		}
	}

	// Every other schedule always runs when its expression fires.
	for _, other := range []store.Schedule{
		store.ManualSchedule(),
		{Kind: store.ScheduleDaily, Time: "02:00"},
		{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 30},
		{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"},
	} {
		if !other.ShouldRun(time.Date(2026, 2, 15, 1, 0, 0, 0, time.UTC)) {
			t.Errorf("ShouldRun(%+v) = false on an ordinary day", other)
		}
	}
}

// TestScheduleJSON pins the wire shape: exactly the fields of the schedule's
// kind, no more, and a bare string still decodes so existing automation keeps
// working.
func TestScheduleJSON(t *testing.T) {
	for _, c := range []struct {
		schedule store.Schedule
		want     string
	}{
		{store.ManualSchedule(), `{"kind":"manual"}`},
		{store.Schedule{}, `{"kind":"manual"}`},
		{store.Schedule{Kind: store.ScheduleHourly, Minute: 30}, `{"kind":"hourly","minute":30}`},
		// A zero minute is a value, not an absence.
		{store.Schedule{Kind: store.ScheduleHourly}, `{"kind":"hourly","minute":0}`},
		{store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}, `{"kind":"daily","time":"02:00"}`},
		{store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{6, 0}},
			`{"kind":"weekly","time":"03:00","weekdays":[0,6]}`},
		{store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 1},
			`{"kind":"monthly","time":"01:00","dayOfMonth":1}`},
		{store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"},
			`{"kind":"advanced","cron":"*/15 * * * *"}`},
	} {
		raw, err := json.Marshal(c.schedule)
		if err != nil {
			t.Fatalf("marshal %+v: %v", c.schedule, err)
		}
		if string(raw) != c.want {
			t.Errorf("marshal %+v = %s, want %s", c.schedule, raw, c.want)
		}
		var back store.Schedule
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if !reflect.DeepEqual(back, c.schedule.Normalized()) {
			t.Errorf("round trip of %s = %+v, want %+v", raw, back, c.schedule.Normalized())
		}
	}

	// A bare string is still accepted on input and is treated as the legacy
	// form: "manual", a preset-expressible cron, or a custom expression.
	for _, c := range []struct {
		in   string
		want store.Schedule
	}{
		{`"manual"`, store.ManualSchedule()},
		{`""`, store.ManualSchedule()},
		{`"0 2 * * *"`, store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}},
		{`"*/15 * * * *"`, store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"}},
		{`null`, store.ManualSchedule()},
	} {
		var got store.Schedule
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want.Normalized()) {
			t.Errorf("unmarshal %s = %+v, want %+v", c.in, got, c.want.Normalized())
		}
	}
}

// TestParseLegacySchedule covers the conversion the database migration applies:
// a cron a preset can express becomes that preset, anything else stays advanced
// with its expression intact — so no upgraded job's timing ever moves.
func TestParseLegacySchedule(t *testing.T) {
	for _, c := range []struct {
		in   string
		want store.Schedule
	}{
		{"manual", store.ManualSchedule()},
		{"", store.ManualSchedule()},
		{"   ", store.ManualSchedule()},
		{"0 * * * *", store.Schedule{Kind: store.ScheduleHourly, Minute: 0}},
		{"30 * * * *", store.Schedule{Kind: store.ScheduleHourly, Minute: 30}},
		{"0 2 * * *", store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}},
		{"0 3 * * 0", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0}}},
		{"0 3 * * 0,6", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0, 6}}},
		{"0 1 1 * *", store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 1}},
		// The expression this package generates for "last day" round trips back
		// into the preset rather than into a custom schedule.
		{"0 1 28-31 * *", store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 31}},
		// Everything a preset cannot express keeps its expression.
		{"*/15 * * * *", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"}},
		{"0 2 * * 1-5", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "0 2 * * 1-5"}},
		{"0 2 1 1 *", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "0 2 1 1 *"}},
		{"0 2 1 * 0", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "0 2 1 * 0"}},
		{"@daily", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "@daily"}},
		{"not a cron", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "not a cron"}},
	} {
		got := store.ParseLegacySchedule(c.in)
		if !reflect.DeepEqual(got, c.want.Normalized()) {
			t.Errorf("ParseLegacySchedule(%q) = %+v, want %+v", c.in, got, c.want.Normalized())
		}
		// Whatever the conversion produced must still fire when the old
		// expression did.
		if c.in != "" && c.in != "   " && c.in != "manual" && c.in != "not a cron" {
			if want, ok := normalisedCron(c.in); ok && got.Cron() != want {
				t.Errorf("ParseLegacySchedule(%q).Cron() = %q, want %q", c.in, got.Cron(), want)
			}
		}
	}
}

// normalisedCron returns the expression a converted schedule must still produce
// for the inputs where the two are literally the same string.
func normalisedCron(in string) (string, bool) {
	switch in {
	case "0 1 28-31 * *":
		return in, true // the last-day window round trips exactly
	default:
		return in, !strings.HasPrefix(in, "@")
	}
}

// TestJobScheduleRoundTrip proves a job's schedule survives the database.
func TestJobScheduleRoundTrip(t *testing.T) {
	st, _ := open(t)
	ctx := t.Context()

	for _, want := range []store.Schedule{
		store.ManualSchedule(),
		{Kind: store.ScheduleHourly, Minute: 30},
		{Kind: store.ScheduleDaily, Time: "02:00"},
		{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0, 6}},
		{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 31},
		{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"},
	} {
		job, err := st.CreateJob(ctx, &store.Job{
			Name: "sched-" + want.Kind, Kind: store.SourceVM, TargetID: "t1",
			Schedule: want, Retention: 3, Enabled: true,
			Sources: store.JobSources{{HostID: "h1", VMID: 100}},
		})
		if err != nil {
			t.Fatalf("create job with %+v: %v", want, err)
		}
		loaded, err := st.JobByID(ctx, job.ID)
		if err != nil {
			t.Fatalf("load job: %v", err)
		}
		if !reflect.DeepEqual(loaded.Schedule, want.Normalized()) {
			t.Errorf("schedule round trip = %+v, want %+v", loaded.Schedule, want.Normalized())
		}

		// And through an update.
		loaded.Schedule = store.Schedule{Kind: store.ScheduleDaily, Time: "05:15"}
		if err := st.UpdateJob(ctx, loaded); err != nil {
			t.Fatalf("update job schedule: %v", err)
		}
		again, err := st.JobByID(ctx, job.ID)
		if err != nil {
			t.Fatalf("reload job: %v", err)
		}
		if again.Schedule.Kind != store.ScheduleDaily || again.Schedule.Time != "05:15" {
			t.Errorf("updated schedule = %+v", again.Schedule)
		}
	}
}
