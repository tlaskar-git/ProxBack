package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule kinds. Operators pick a schedule the way they think about it; the
// cron expression the scheduler runs on is derived from it and never appears in
// the product surface — except for "advanced", the deliberate escape hatch.
const (
	ScheduleManual   = "manual"
	ScheduleHourly   = "hourly"
	ScheduleDaily    = "daily"
	ScheduleWeekly   = "weekly"
	ScheduleMonthly  = "monthly"
	ScheduleAdvanced = "advanced"
)

// LastDayOfMonth is the dayOfMonth value that means "run on the last day of the
// month", whatever its length. It is the top of the accepted 1–31 range because
// that is how an operator expresses the intent.
const LastDayOfMonth = 31

// lastDayCronDays is the day-of-month field Cron uses for LastDayOfMonth. Cron
// cannot express "last day", so the expression fires on every day that could be
// the last one and Schedule.ShouldRun discards the firings that are not.
const lastDayCronDays = "28-31"

// weekdayNames renders a weekday number the way the schedule label shows it.
// Index 0 is Sunday, matching cron (and the API contract).
var weekdayNames = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// Schedule is a job's structured schedule. Only the fields belonging to Kind
// carry meaning, and only those are serialised:
//
//	{"kind":"manual"}
//	{"kind":"hourly","minute":30}
//	{"kind":"daily","time":"02:00"}
//	{"kind":"weekly","time":"03:00","weekdays":[0,6]}
//	{"kind":"monthly","time":"01:00","dayOfMonth":1}
//	{"kind":"advanced","cron":"*/15 * * * *"}
//
// All times are the server's local timezone, which GET /api/settings reports.
type Schedule struct {
	Kind string
	// Minute is the minute past the hour an hourly schedule fires (0–59).
	Minute int
	// Time is "HH:MM" in 24 hour form for daily, weekly and monthly schedules.
	Time string
	// Weekdays are the days a weekly schedule fires, 0 = Sunday … 6 = Saturday.
	Weekdays []int
	// DayOfMonth is the day a monthly schedule fires (1–31; LastDayOfMonth means
	// the last day of whatever month it is).
	DayOfMonth int
	// CronExpr is the raw expression of an advanced schedule. It is exposed as
	// "cron" on the wire; the derived expression of every kind comes from Cron().
	CronExpr string
}

// ManualSchedule returns the schedule of a job that only ever runs on demand.
func ManualSchedule() Schedule { return Schedule{Kind: ScheduleManual} }

// scheduleJSON is the wire shape. The pointers and omitempty tags are what keep
// a schedule's JSON to exactly the fields its kind defines — including
// {"minute":0} and {"dayOfMonth":0} not collapsing into absence.
type scheduleJSON struct {
	Kind       string `json:"kind"`
	Minute     *int   `json:"minute,omitempty"`
	Time       string `json:"time,omitempty"`
	Weekdays   []int  `json:"weekdays,omitempty"`
	DayOfMonth *int   `json:"dayOfMonth,omitempty"`
	Cron       string `json:"cron,omitempty"`
}

// MarshalJSON renders the object shape the API contract specifies, emitting only
// the fields that belong to the schedule's kind.
func (s Schedule) MarshalJSON() ([]byte, error) {
	out := scheduleJSON{Kind: s.Kind}
	if out.Kind == "" {
		out.Kind = ScheduleManual
	}
	switch out.Kind {
	case ScheduleHourly:
		minute := s.Minute
		out.Minute = &minute
	case ScheduleDaily:
		out.Time = s.Time
	case ScheduleWeekly:
		out.Time = s.Time
		out.Weekdays = normalizeWeekdays(s.Weekdays)
		if out.Weekdays == nil {
			out.Weekdays = []int{}
		}
	case ScheduleMonthly:
		out.Time = s.Time
		day := s.DayOfMonth
		out.DayOfMonth = &day
	case ScheduleAdvanced:
		out.Cron = s.CronExpr
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts the schedule object and, for backwards compatibility,
// the bare string earlier releases used ("manual" or a cron expression). The
// string form keeps existing automation working.
func (s *Schedule) UnmarshalJSON(b []byte) error {
	trimmed := trimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*s = ManualSchedule()
		return nil
	}
	if trimmed[0] == '"' {
		var raw string
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return fmt.Errorf("decode schedule: %w", err)
		}
		*s = ParseLegacySchedule(raw)
		return nil
	}
	var in scheduleJSON
	dec := json.NewDecoder(strings.NewReader(string(trimmed)))
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("decode schedule: %w", err)
	}
	out := Schedule{
		Kind:     strings.ToLower(strings.TrimSpace(in.Kind)),
		Time:     strings.TrimSpace(in.Time),
		Weekdays: in.Weekdays,
		CronExpr: strings.TrimSpace(in.Cron),
	}
	if in.Minute != nil {
		out.Minute = *in.Minute
	}
	if in.DayOfMonth != nil {
		out.DayOfMonth = *in.DayOfMonth
	}
	*s = out
	return nil
}

// Normalized returns the schedule with its fields tidied: an empty kind becomes
// "manual" and weekdays are de-duplicated and sorted.
func (s Schedule) Normalized() Schedule {
	if s.Kind == "" {
		s.Kind = ScheduleManual
	}
	s.Kind = strings.ToLower(strings.TrimSpace(s.Kind))
	s.Time = strings.TrimSpace(s.Time)
	s.CronExpr = strings.TrimSpace(s.CronExpr)
	s.Weekdays = normalizeWeekdays(s.Weekdays)
	return s
}

// normalizeWeekdays de-duplicates and sorts weekday numbers. Out-of-range values
// are kept so validation can complain about them by name.
func normalizeWeekdays(days []int) []int {
	if len(days) == 0 {
		return nil
	}
	out := make([]int, 0, len(days))
	seen := make(map[int]struct{}, len(days))
	for _, d := range days {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

// Validate reports why a schedule cannot be used, with a message an operator can
// act on. The API turns a non-nil result into a 400.
func (s Schedule) Validate() error {
	s = s.Normalized()
	switch s.Kind {
	case ScheduleManual:
		return nil
	case ScheduleHourly:
		if s.Minute < 0 || s.Minute > 59 {
			return fmt.Errorf("schedule minute must be between 0 and 59, got %d", s.Minute)
		}
		return nil
	case ScheduleDaily:
		return validateClockTime(s.Time)
	case ScheduleWeekly:
		if err := validateClockTime(s.Time); err != nil {
			return err
		}
		if len(s.Weekdays) == 0 {
			return fmt.Errorf("a weekly schedule needs at least one weekday (0 = Sunday … 6 = Saturday)")
		}
		for _, d := range s.Weekdays {
			if d < 0 || d > 6 {
				return fmt.Errorf("schedule weekdays must be between 0 and 6 (0 = Sunday), got %d", d)
			}
		}
		return nil
	case ScheduleMonthly:
		if err := validateClockTime(s.Time); err != nil {
			return err
		}
		if s.DayOfMonth < 1 || s.DayOfMonth > LastDayOfMonth {
			return fmt.Errorf("schedule dayOfMonth must be between 1 and %d, got %d", LastDayOfMonth, s.DayOfMonth)
		}
		return nil
	case ScheduleAdvanced:
		if s.CronExpr == "" {
			return fmt.Errorf(`an advanced schedule needs a "cron" expression`)
		}
		if _, err := cron.ParseStandard(s.CronExpr); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", s.CronExpr, err)
		}
		return nil
	default:
		return fmt.Errorf(`schedule kind must be one of "manual", "hourly", "daily", "weekly", "monthly" or "advanced", got %q`, s.Kind)
	}
}

// validateClockTime enforces the "HH:MM" 24 hour form the UI sends.
func validateClockTime(v string) error {
	h, m, ok := parseClockTime(v)
	if !ok || h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf(`schedule time must be "HH:MM" in 24 hour form, got %q`, v)
	}
	return nil
}

// parseClockTime splits a strict "HH:MM" into its parts. Both halves must be
// two digits: "9:30" and "+9:30" are rejected so the UI and the API agree on
// exactly one representation.
func parseClockTime(v string) (hour, minute int, ok bool) {
	if len(v) != 5 || v[2] != ':' || !allDigits(v[:2]) || !allDigits(v[3:]) {
		return 0, 0, false
	}
	h, err := strconv.Atoi(v[:2])
	if err != nil {
		return 0, 0, false
	}
	m, err := strconv.Atoi(v[3:])
	if err != nil {
		return 0, 0, false
	}
	return h, m, true
}

// allDigits reports whether s is a non-empty run of ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Cron derives the expression the scheduler runs the job on. It is empty for a
// manual schedule, which is never registered with cron at all.
//
// The monthly last-day case is the one thing cron cannot express: there is no
// "L" in the standard five field syntax and robfig/cron does not implement one.
// LastDayOfMonth therefore fires on days 28–31 and Schedule.ShouldRun discards
// the firings that are not actually the month's last day — so February gets one
// run on the 28th (29th in a leap year), April one on the 30th and January one
// on the 31st. Everything that consumes the expression (the scheduler and
// NextRun) must consult ShouldRun.
func (s Schedule) Cron() string {
	s = s.Normalized()
	hour, minute, _ := parseClockTime(s.Time)
	switch s.Kind {
	case ScheduleHourly:
		return fmt.Sprintf("%d * * * *", s.Minute)
	case ScheduleDaily:
		return fmt.Sprintf("%d %d * * *", minute, hour)
	case ScheduleWeekly:
		days := make([]string, 0, len(s.Weekdays))
		for _, d := range s.Weekdays {
			days = append(days, strconv.Itoa(d))
		}
		if len(days) == 0 {
			return ""
		}
		return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(days, ","))
	case ScheduleMonthly:
		if s.DayOfMonth >= LastDayOfMonth {
			return fmt.Sprintf("%d %d %s * *", minute, hour, lastDayCronDays)
		}
		if s.DayOfMonth < 1 {
			return ""
		}
		return fmt.Sprintf("%d %d %d * *", minute, hour, s.DayOfMonth)
	case ScheduleAdvanced:
		return s.CronExpr
	default: // manual, and anything unrecognised
		return ""
	}
}

// ShouldRun reports whether a firing at t is a real occurrence of the schedule.
// Only the monthly last-day schedule can answer false: its expression covers
// days 28–31 and this discards the days that are not the last of their month.
func (s Schedule) ShouldRun(t time.Time) bool {
	if s.Kind != ScheduleMonthly || s.DayOfMonth < LastDayOfMonth {
		return true
	}
	return t.Day() == daysInMonth(t)
}

// daysInMonth returns the length of t's month. Day 0 of the next month is the
// last day of this one, which also handles February in a leap year.
func daysInMonth(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, t.Location()).Day()
}

// Label renders the English summary the UI displays verbatim as scheduleLabel.
func (s Schedule) Label() string {
	s = s.Normalized()
	switch s.Kind {
	case ScheduleHourly:
		return fmt.Sprintf("Every hour at :%02d", s.Minute)
	case ScheduleDaily:
		return "Daily at " + s.Time
	case ScheduleWeekly:
		names := make([]string, 0, len(s.Weekdays))
		for _, d := range s.Weekdays {
			if d < 0 || d > 6 {
				continue
			}
			names = append(names, weekdayNames[d])
		}
		if len(names) == 0 {
			return "Weekly at " + s.Time
		}
		return fmt.Sprintf("Weekly on %s at %s", strings.Join(names, ", "), s.Time)
	case ScheduleMonthly:
		if s.DayOfMonth >= LastDayOfMonth {
			return "Monthly on the last day at " + s.Time
		}
		return fmt.Sprintf("Monthly on day %d at %s", s.DayOfMonth, s.Time)
	case ScheduleAdvanced:
		return fmt.Sprintf("Custom (%s)", s.CronExpr)
	default:
		return "Manual"
	}
}

// ---------------------------------------------------------------- legacy form

// ParseLegacySchedule converts the pre-v0.4.0 schedule column — "manual", ""
// or a bare cron expression — into a structured schedule. A cron expression
// that a preset can express becomes that preset, so a job scheduled "0 2 * * *"
// by an older release shows up in the UI as "Daily at 02:00" rather than as a
// custom expression; anything else keeps its expression as an advanced
// schedule, so no job's timing ever changes across the upgrade.
func ParseLegacySchedule(raw string) Schedule {
	spec := strings.TrimSpace(raw)
	if spec == "" || spec == ScheduleManual {
		return ManualSchedule()
	}
	if preset, ok := schedulePreset(spec); ok {
		return preset
	}
	return Schedule{Kind: ScheduleAdvanced, CronExpr: spec}
}

// schedulePreset recognises the cron expressions the presets generate, which is
// what lets an upgraded job present itself as "Daily at 02:00" instead of
// "Custom (0 2 * * *)". Anything with a step, range or list outside the weekly
// weekday field is not preset-expressible and stays advanced.
func schedulePreset(spec string) (Schedule, bool) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return Schedule{}, false
	}
	minuteField, hourField, domField, monthField, dowField := fields[0], fields[1], fields[2], fields[3], fields[4]
	if monthField != "*" {
		return Schedule{}, false
	}
	minute, ok := cronInt(minuteField, 0, 59)
	if !ok {
		return Schedule{}, false
	}
	if hourField == "*" {
		// Every hour: only meaningful when no day restriction narrows it.
		if domField == "*" && dowField == "*" {
			return Schedule{Kind: ScheduleHourly, Minute: minute}, true
		}
		return Schedule{}, false
	}
	hour, ok := cronInt(hourField, 0, 23)
	if !ok {
		return Schedule{}, false
	}
	clock := fmt.Sprintf("%02d:%02d", hour, minute)
	switch {
	case domField == "*" && dowField == "*":
		return Schedule{Kind: ScheduleDaily, Time: clock}, true
	case domField == "*":
		days, ok := cronIntList(dowField, 0, 6)
		if !ok {
			return Schedule{}, false
		}
		return Schedule{Kind: ScheduleWeekly, Time: clock, Weekdays: days}, true
	case dowField == "*":
		// The expression this package generates for "last day of the month"
		// round-trips back into that preset rather than into a custom schedule.
		if domField == lastDayCronDays {
			return Schedule{Kind: ScheduleMonthly, Time: clock, DayOfMonth: LastDayOfMonth}, true
		}
		day, ok := cronInt(domField, 1, 31)
		if !ok {
			return Schedule{}, false
		}
		return Schedule{Kind: ScheduleMonthly, Time: clock, DayOfMonth: day}, true
	default:
		return Schedule{}, false
	}
}

// cronInt parses a plain integer cron field within bounds. Steps, ranges and
// anything else a preset cannot express are rejected.
func cronInt(field string, min, max int) (int, bool) {
	if !allDigits(field) {
		return 0, false
	}
	n, err := strconv.Atoi(field)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

// cronIntList parses a comma separated list of plain integers within bounds.
func cronIntList(field string, min, max int) ([]int, bool) {
	parts := strings.Split(field, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, ok := cronInt(strings.TrimSpace(p), min, max)
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return normalizeWeekdays(out), true
}

// ---------------------------------------------------------------- persistence

// encodeSchedule renders a schedule for the jobs.schedule column.
func encodeSchedule(s Schedule) (string, error) {
	raw, err := json.Marshal(s.Normalized())
	if err != nil {
		return "", fmt.Errorf("encode job schedule: %w", err)
	}
	return string(raw), nil
}

// decodeSchedule reads the jobs.schedule column. The column held "manual" or a
// bare cron expression before v0.4.0 and holds the JSON object after it, so the
// two are told apart by the leading brace. Databases are converted on open, but
// a row written by an older binary running against the same file still reads.
func decodeSchedule(raw string) Schedule {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		var s Schedule
		if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
			return s.Normalized()
		}
		// Unreadable JSON is not worth failing a listing over; fall through and
		// treat it as the legacy form, which ends at "manual".
	}
	return ParseLegacySchedule(trimmed)
}
