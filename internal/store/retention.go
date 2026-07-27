package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Retention reasons. They are the vocabulary the preview endpoint returns and
// the console renders as badges, so they are part of the API contract.
const (
	RetainLast    = "last"
	RetainDaily   = "daily"
	RetainWeekly  = "weekly"
	RetainMonthly = "monthly"
	RetainYearly  = "yearly"
)

// MaxRetentionCount bounds every retention counter. It is generous rather than
// meaningful: past it the number is a typo, not a policy.
const MaxRetentionCount = 9999

// RetentionPolicy is grandfather-father-son retention. A restore point survives
// if *any* rule retains it, so the rules add to one another rather than
// competing: "keep last 7" plus "4 weekly" is 7 recent points and a weekly
// point for each of the last four weeks, whichever of them those turn out to be.
type RetentionPolicy struct {
	KeepLast    int `json:"keepLast"`
	KeepDaily   int `json:"keepDaily"`
	KeepWeekly  int `json:"keepWeekly"`
	KeepMonthly int `json:"keepMonthly"`
	KeepYearly  int `json:"keepYearly"`
}

// KeepLast is the simple policy: the most recent n restore points, whatever day
// they fall on. It is also what a bare integer retention means.
func KeepLast(n int) RetentionPolicy { return RetentionPolicy{KeepLast: n} }

// DefaultRetention is what a job gets when it says nothing.
func DefaultRetention() RetentionPolicy { return KeepLast(7) }

// UnmarshalJSON accepts the object and, for existing jobs and older clients,
// the bare integer earlier releases used — which means keep-last-N.
func (r *RetentionPolicy) UnmarshalJSON(b []byte) error {
	trimmed := trimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*r = DefaultRetention()
		return nil
	}
	if trimmed[0] != '{' {
		var n int
		if err := json.Unmarshal(trimmed, &n); err != nil {
			return fmt.Errorf("decode retention: %w", err)
		}
		*r = KeepLast(n)
		return nil
	}
	type plain RetentionPolicy
	var in plain
	if err := json.Unmarshal(trimmed, &in); err != nil {
		return fmt.Errorf("decode retention: %w", err)
	}
	*r = RetentionPolicy(in)
	return nil
}

// Total is the number of restore points the policy could retain across all its
// rules. Zero means the policy keeps nothing at all.
func (r RetentionPolicy) Total() int {
	return r.KeepLast + r.KeepDaily + r.KeepWeekly + r.KeepMonthly + r.KeepYearly
}

// IsSimple reports whether only keepLast is in play.
func (r RetentionPolicy) IsSimple() bool {
	return r.KeepDaily == 0 && r.KeepWeekly == 0 && r.KeepMonthly == 0 && r.KeepYearly == 0
}

// Validate reports why a retention policy cannot be used, naming the offending
// field the way the API's 400 must.
func (r RetentionPolicy) Validate() error {
	for _, f := range []struct {
		name  string
		value int
	}{
		{"retention.keepLast", r.KeepLast},
		{"retention.keepDaily", r.KeepDaily},
		{"retention.keepWeekly", r.KeepWeekly},
		{"retention.keepMonthly", r.KeepMonthly},
		{"retention.keepYearly", r.KeepYearly},
	} {
		if f.value < 0 || f.value > MaxRetentionCount {
			return fmt.Errorf("%s must be between 0 and %d, got %d", f.name, MaxRetentionCount, f.value)
		}
	}
	return nil
}

// Describe renders the one-line English summary the run log uses.
func (r RetentionPolicy) Describe() string {
	parts := make([]string, 0, 5)
	if r.KeepLast > 0 {
		parts = append(parts, fmt.Sprintf("keep last %d", r.KeepLast))
	}
	for _, f := range []struct {
		n     int
		label string
	}{
		{r.KeepDaily, "daily"}, {r.KeepWeekly, "weekly"},
		{r.KeepMonthly, "monthly"}, {r.KeepYearly, "yearly"},
	} {
		if f.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", f.n, f.label))
		}
	}
	if len(parts) == 0 {
		return "keeps nothing"
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- evaluation

// RetentionPoint is one restore point as retention sees it: an identity and a
// timestamp. Nothing else about a backup can change the decision, which is what
// makes the decision testable without a database.
type RetentionPoint struct {
	ID        string
	CreatedAt time.Time
}

// RetentionDecision is one point's fate. Reasons is empty for a pruned point
// and holds every rule that saved a kept one, in the fixed order
// last, daily, weekly, monthly, yearly.
type RetentionDecision struct {
	ID        string
	CreatedAt time.Time
	Reasons   []string
}

// RetentionPlan is what a policy would do to a set of restore points. It is
// never applied by the function that computes it: the preview endpoint and the
// pruning pass read the same plan.
type RetentionPlan struct {
	Keeps  []RetentionDecision
	Prunes []RetentionDecision
}

// bucketKey renders the calendar period a timestamp belongs to. Weeks are ISO
// weeks, so a week runs Monday to Sunday and the turn of the year does not
// produce a two-day "week".
func bucketKey(t time.Time, reason string) string {
	t = t.UTC()
	switch reason {
	case RetainDaily:
		return t.Format("2006-01-02")
	case RetainWeekly:
		year, week := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	case RetainMonthly:
		return t.Format("2006-01")
	case RetainYearly:
		return t.Format("2006")
	default:
		return ""
	}
}

// EvaluateRetention decides which restore points a policy keeps and why.
//
// The rules are independent and additive — a point survives if any of them
// retains it, and a point retained twice reports both reasons:
//
//   - keepLast keeps the n newest points outright.
//   - keepDaily, keepWeekly, keepMonthly and keepYearly bucket the points by
//     calendar period (UTC day, ISO week, calendar month, calendar year) and
//     keep the *newest point within each period*, for the n most recent periods
//     counted backwards from the most recent point. Periods in which nothing
//     was backed up do not consume a slot: a job that ran on only three of the
//     last twelve weeks still keeps those three under "4 weekly", it does not
//     lose one to an empty week.
//
// The input is not modified and the function touches nothing outside it.
func EvaluateRetention(points []RetentionPoint, policy RetentionPolicy) RetentionPlan {
	sorted := make([]RetentionPoint, len(points))
	copy(sorted, points)
	// Newest first, with the id as the tiebreaker so two points written in the
	// same instant still order deterministically.
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		}
		return sorted[i].ID > sorted[j].ID
	})

	reasons := make(map[string][]string, len(sorted))
	add := func(id, reason string) {
		for _, have := range reasons[id] {
			if have == reason {
				return
			}
		}
		reasons[id] = append(reasons[id], reason)
	}

	if policy.KeepLast > 0 {
		for i := 0; i < len(sorted) && i < policy.KeepLast; i++ {
			add(sorted[i].ID, RetainLast)
		}
	}
	for _, rule := range []struct {
		reason string
		keep   int
	}{
		{RetainDaily, policy.KeepDaily},
		{RetainWeekly, policy.KeepWeekly},
		{RetainMonthly, policy.KeepMonthly},
		{RetainYearly, policy.KeepYearly},
	} {
		if rule.keep <= 0 {
			continue
		}
		seen := make(map[string]struct{}, rule.keep)
		// sorted is newest first, so the first point of a bucket is that
		// period's newest, and buckets arrive newest first too.
		for _, p := range sorted {
			key := bucketKey(p.CreatedAt, rule.reason)
			if _, done := seen[key]; done {
				continue
			}
			if len(seen) >= rule.keep {
				break
			}
			seen[key] = struct{}{}
			add(p.ID, rule.reason)
		}
	}

	// The reasons of a kept point are reported in rule order, never in the
	// order the rules happened to run.
	order := []string{RetainLast, RetainDaily, RetainWeekly, RetainMonthly, RetainYearly}
	plan := RetentionPlan{Keeps: []RetentionDecision{}, Prunes: []RetentionDecision{}}
	for _, p := range sorted {
		got := reasons[p.ID]
		if len(got) == 0 {
			plan.Prunes = append(plan.Prunes, RetentionDecision{
				ID: p.ID, CreatedAt: p.CreatedAt, Reasons: []string{},
			})
			continue
		}
		ordered := make([]string, 0, len(got))
		for _, reason := range order {
			for _, have := range got {
				if have == reason {
					ordered = append(ordered, reason)
					break
				}
			}
		}
		plan.Keeps = append(plan.Keeps, RetentionDecision{
			ID: p.ID, CreatedAt: p.CreatedAt, Reasons: ordered,
		})
	}
	return plan
}

// ---------------------------------------------------------------- persistence

// encodeRetention renders a retention policy for the jobs.retention_policy
// column.
func encodeRetention(r RetentionPolicy) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode job retention: %w", err)
	}
	return string(raw), nil
}

// decodeRetention reads the retention columns. Databases written before v0.5.0
// hold only the integer, which means keep-last-N; the object column wins when
// it is present. Both are written on every save, so a row stays readable by an
// older binary running against the same file.
func decodeRetention(object string, legacy int) RetentionPolicy {
	trimmed := strings.TrimSpace(object)
	if trimmed == "" {
		return KeepLast(legacy)
	}
	var r RetentionPolicy
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return KeepLast(legacy)
	}
	return r
}
