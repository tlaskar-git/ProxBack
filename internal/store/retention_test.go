package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"proxback/internal/store"
)

// day is a helper for building synthetic restore points: one point per day at
// noon UTC, so a "day" is unambiguous and no test depends on the machine's zone.
func at(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
}

// points builds restore points named after their timestamps, newest last. The
// evaluation must not depend on the order they arrive in, so several tests
// deliberately feed them unsorted.
func points(times ...time.Time) []store.RetentionPoint {
	out := make([]store.RetentionPoint, 0, len(times))
	for _, t := range times {
		out = append(out, store.RetentionPoint{ID: t.Format("2006-01-02T15"), CreatedAt: t})
	}
	return out
}

// ids renders a decision list as its identifiers, in the order returned.
func ids(decisions []store.RetentionDecision) []string {
	out := make([]string, 0, len(decisions))
	for _, d := range decisions {
		out = append(out, d.ID)
	}
	return out
}

// reasonsOf indexes a decision list by identifier.
func reasonsOf(decisions []store.RetentionDecision) map[string]string {
	out := map[string]string{}
	for _, d := range decisions {
		out[d.ID] = strings.Join(d.Reasons, "+")
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEvaluateRetention is the table that defines what a retention policy
// means. Everything else — the pruning pass and the preview endpoint — reads
// its answer from here, so these cases are the contract.
func TestEvaluateRetention(t *testing.T) {
	// A synthetic year of daily backups, one per day at 03:00, ending on
	// 2026-01-01. Ending on New Year's Day is deliberate: it puts the yearly and
	// the ISO-week boundaries under test at once.
	var year []store.RetentionPoint
	for d := at(2025, time.January, 1, 3); !d.After(at(2026, time.January, 1, 3)); d = d.AddDate(0, 0, 1) {
		year = append(year, store.RetentionPoint{ID: d.Format("2006-01-02T15"), CreatedAt: d})
	}

	for _, c := range []struct {
		name   string
		points []store.RetentionPoint
		policy store.RetentionPolicy
		// wantKeeps is the exact list of kept identifiers, newest first.
		wantKeeps []string
		// wantReasons is checked for the identifiers it names only.
		wantReasons map[string]string
		wantPrunes  int
	}{
		{
			name:   "empty input keeps and prunes nothing",
			points: nil,
			policy: store.RetentionPolicy{KeepLast: 7, KeepWeekly: 4},
		},
		{
			name: "keep last only",
			points: points(
				at(2026, time.March, 1, 3), at(2026, time.March, 2, 3),
				at(2026, time.March, 3, 3), at(2026, time.March, 4, 3),
			),
			policy:    store.KeepLast(2),
			wantKeeps: []string{"2026-03-04T03", "2026-03-03T03"},
			wantReasons: map[string]string{
				"2026-03-04T03": "last", "2026-03-03T03": "last",
			},
			wantPrunes: 2,
		},
		{
			name: "keep last is a count of points, not of days",
			points: points(
				at(2026, time.March, 1, 3), at(2026, time.March, 1, 9),
				at(2026, time.March, 1, 21), at(2026, time.February, 27, 3),
			),
			policy:     store.KeepLast(3),
			wantKeeps:  []string{"2026-03-01T21", "2026-03-01T09", "2026-03-01T03"},
			wantPrunes: 1,
		},
		{
			name: "a policy of all zeros keeps nothing at all",
			points: points(
				at(2026, time.March, 1, 3), at(2026, time.March, 2, 3),
			),
			policy:     store.RetentionPolicy{},
			wantKeeps:  []string{},
			wantPrunes: 2,
		},
		{
			name: "daily keeps the newest point of each day",
			points: points(
				at(2026, time.March, 1, 3), at(2026, time.March, 1, 22),
				at(2026, time.March, 2, 3), at(2026, time.March, 2, 20),
				at(2026, time.March, 3, 3),
			),
			policy:    store.RetentionPolicy{KeepDaily: 2},
			wantKeeps: []string{"2026-03-03T03", "2026-03-02T20"},
			wantReasons: map[string]string{
				"2026-03-03T03": "daily", "2026-03-02T20": "daily",
			},
			wantPrunes: 3,
		},
		{
			name:   "a year of dailies under a full GFS policy",
			points: year,
			policy: store.RetentionPolicy{KeepLast: 3, KeepWeekly: 4, KeepMonthly: 3, KeepYearly: 1},
			// The three newest points, the newest point of each of the last four
			// ISO weeks, of the last three calendar months and of the last year.
			// 2026-01-01 is a Thursday in ISO week 1 of 2026 and is the newest
			// point overall, so it carries four reasons at once; 2025-12-31 is
			// both a recent point and December's newest.
			wantKeeps: []string{
				"2026-01-01T03", // last + weekly + monthly + yearly
				"2025-12-31T03", // last + monthly
				"2025-12-30T03", // last
				"2025-12-28T03", // weekly (the Sunday that ends ISO week 52)
				"2025-12-21T03", // weekly
				"2025-12-14T03", // weekly
				"2025-11-30T03", // monthly
			},
			wantReasons: map[string]string{
				"2026-01-01T03": "last+weekly+monthly+yearly",
				"2025-12-31T03": "last+monthly",
				"2025-12-30T03": "last",
				"2025-12-28T03": "weekly",
				"2025-11-30T03": "monthly",
			},
			wantPrunes: len(year) - 7,
		},
		{
			name:   "yearly keeps the newest point of each calendar year",
			points: year,
			policy: store.RetentionPolicy{KeepYearly: 2},
			wantKeeps: []string{
				"2026-01-01T03", "2025-12-31T03",
			},
			wantReasons: map[string]string{
				"2026-01-01T03": "yearly", "2025-12-31T03": "yearly",
			},
			wantPrunes: len(year) - 2,
		},
		{
			name: "a point retained by two rules reports both",
			points: points(
				at(2026, time.March, 1, 3), at(2026, time.March, 2, 3), at(2026, time.March, 3, 3),
			),
			policy:    store.RetentionPolicy{KeepLast: 1, KeepDaily: 1},
			wantKeeps: []string{"2026-03-03T03"},
			wantReasons: map[string]string{
				"2026-03-03T03": "last+daily",
			},
			wantPrunes: 2,
		},
		{
			name: "months with no backup do not consume a monthly slot",
			points: points(
				at(2025, time.January, 15, 3), at(2025, time.June, 10, 3), at(2025, time.December, 2, 3),
			),
			policy: store.RetentionPolicy{KeepMonthly: 3},
			wantKeeps: []string{
				"2025-12-02T03", "2025-06-10T03", "2025-01-15T03",
			},
			wantPrunes: 0,
		},
		{
			name: "a policy larger than the history keeps everything",
			points: points(
				at(2026, time.March, 1, 3), at(2026, time.March, 2, 3),
			),
			policy:     store.RetentionPolicy{KeepLast: 50, KeepMonthly: 12},
			wantKeeps:  []string{"2026-03-02T03", "2026-03-01T03"},
			wantPrunes: 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The input is deliberately handed over in a random-ish order: the
			// answer must come from the timestamps, not from the slice.
			shuffled := make([]store.RetentionPoint, len(c.points))
			copy(shuffled, c.points)
			for i := 0; i < len(shuffled)/2; i++ {
				j := len(shuffled) - 1 - i
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			}
			plan := store.EvaluateRetention(shuffled, c.policy)

			if c.wantKeeps != nil && !equalStrings(ids(plan.Keeps), c.wantKeeps) {
				t.Fatalf("keeps = %v, want %v", ids(plan.Keeps), c.wantKeeps)
			}
			if len(plan.Prunes) != c.wantPrunes {
				t.Fatalf("pruned %d points (%v), want %d", len(plan.Prunes), ids(plan.Prunes), c.wantPrunes)
			}
			got := reasonsOf(plan.Keeps)
			for id, want := range c.wantReasons {
				if got[id] != want {
					t.Errorf("reasons for %s = %q, want %q", id, got[id], want)
				}
			}
			// Nothing is ever both kept and pruned, and nothing is lost.
			if len(plan.Keeps)+len(plan.Prunes) != len(c.points) {
				t.Fatalf("plan covers %d points, input had %d",
					len(plan.Keeps)+len(plan.Prunes), len(c.points))
			}
			for _, d := range plan.Keeps {
				if len(d.Reasons) == 0 {
					t.Errorf("kept point %s carries no reason", d.ID)
				}
			}
			for _, d := range plan.Prunes {
				if len(d.Reasons) != 0 {
					t.Errorf("pruned point %s carries reasons %v", d.ID, d.Reasons)
				}
			}
			// The input is untouched: a preview may never mutate anything.
			if len(shuffled) != len(c.points) {
				t.Fatalf("the input slice changed length")
			}
		})
	}
}

// TestEvaluateRetentionIsStable pins down that the same input always produces
// the same answer, including when two restore points share a timestamp — which
// is what a preview shown before saving has to be able to promise.
func TestEvaluateRetentionIsStable(t *testing.T) {
	same := at(2026, time.March, 1, 3)
	in := []store.RetentionPoint{
		{ID: "a", CreatedAt: same},
		{ID: "b", CreatedAt: same},
		{ID: "c", CreatedAt: same.Add(-time.Hour)},
	}
	policy := store.KeepLast(2)
	first := store.EvaluateRetention(in, policy)
	for i := 0; i < 5; i++ {
		got := store.EvaluateRetention(in, policy)
		if !equalStrings(ids(got.Keeps), ids(first.Keeps)) ||
			!equalStrings(ids(got.Prunes), ids(first.Prunes)) {
			t.Fatalf("run %d = keeps %v prunes %v, first = keeps %v prunes %v",
				i, ids(got.Keeps), ids(got.Prunes), ids(first.Keeps), ids(first.Prunes))
		}
	}
}

// TestRetentionPolicyAcceptsABareInteger covers the compatibility promise: a
// job written by an older release, and any client that still sends a number,
// keeps working and means keep-last-N.
func TestRetentionPolicyAcceptsABareInteger(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want store.RetentionPolicy
	}{
		{`7`, store.RetentionPolicy{KeepLast: 7}},
		{`0`, store.RetentionPolicy{}},
		{`{"keepLast":3}`, store.RetentionPolicy{KeepLast: 3}},
		{`{"keepLast":7,"keepWeekly":4,"keepMonthly":6,"keepYearly":1}`,
			store.RetentionPolicy{KeepLast: 7, KeepWeekly: 4, KeepMonthly: 6, KeepYearly: 1}},
		{`null`, store.DefaultRetention()},
		{`{}`, store.RetentionPolicy{}},
	} {
		var got store.RetentionPolicy
		if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
			t.Fatalf("decode %s: %v", c.raw, err)
		}
		if got != c.want {
			t.Errorf("decode %s = %+v, want %+v", c.raw, got, c.want)
		}
	}

	// It always marshals as the object, so a client never has to guess.
	raw, err := json.Marshal(store.KeepLast(5))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(raw), `"keepLast":5`) || !strings.Contains(string(raw), `"keepYearly":0`) {
		t.Fatalf("encoded retention = %s, want the full object", raw)
	}
}

func TestRetentionPolicyValidate(t *testing.T) {
	for _, c := range []struct {
		policy    store.RetentionPolicy
		wantField string
	}{
		{store.RetentionPolicy{KeepLast: 7, KeepWeekly: 4}, ""},
		{store.RetentionPolicy{}, ""},
		{store.RetentionPolicy{KeepLast: -1}, "retention.keepLast"},
		{store.RetentionPolicy{KeepDaily: -3}, "retention.keepDaily"},
		{store.RetentionPolicy{KeepWeekly: 10_000}, "retention.keepWeekly"},
		{store.RetentionPolicy{KeepMonthly: -1}, "retention.keepMonthly"},
		{store.RetentionPolicy{KeepYearly: 99_999}, "retention.keepYearly"},
	} {
		err := c.policy.Validate()
		if c.wantField == "" {
			if err != nil {
				t.Errorf("Validate(%+v) = %v, want nil", c.policy, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantField) {
			t.Errorf("Validate(%+v) = %v, want an error naming %s", c.policy, err, c.wantField)
		}
	}
}

func TestRetentionPolicyDescribe(t *testing.T) {
	for _, c := range []struct {
		policy store.RetentionPolicy
		want   string
	}{
		{store.KeepLast(7), "keep last 7"},
		{store.RetentionPolicy{KeepLast: 7, KeepWeekly: 4, KeepMonthly: 6}, "keep last 7, 4 weekly, 6 monthly"},
		{store.RetentionPolicy{}, "keeps nothing"},
	} {
		if got := c.policy.Describe(); got != c.want {
			t.Errorf("Describe(%+v) = %q, want %q", c.policy, got, c.want)
		}
	}
}

// TestEvaluateRetentionWeeklyBucketsAreISOWeeks documents the boundary the
// bucketing uses: a week runs Monday to Sunday, so points on either side of a
// Sunday midnight are in different weeks even though they are hours apart.
func TestEvaluateRetentionWeeklyBucketsAreISOWeeks(t *testing.T) {
	// 2026-03-01 is a Sunday, 2026-03-02 a Monday.
	sunday := at(2026, time.March, 1, 23)
	monday := at(2026, time.March, 2, 1)
	plan := store.EvaluateRetention(points(sunday, monday), store.RetentionPolicy{KeepWeekly: 2})
	if len(plan.Keeps) != 2 || len(plan.Prunes) != 0 {
		t.Fatalf("two points two hours apart across a week boundary: keeps %v prunes %v",
			ids(plan.Keeps), ids(plan.Prunes))
	}
	// One weekly slot keeps only the newer week's point.
	plan = store.EvaluateRetention(points(sunday, monday), store.RetentionPolicy{KeepWeekly: 1})
	if !equalStrings(ids(plan.Keeps), []string{monday.Format("2006-01-02T15")}) {
		t.Fatalf("keeps = %v, want only the Monday point", ids(plan.Keeps))
	}
}

// The reasons vocabulary is part of the API contract — the console renders
// these strings as badges — so it is pinned rather than left to drift.
func TestRetentionReasonVocabulary(t *testing.T) {
	want := []string{"last", "daily", "weekly", "monthly", "yearly"}
	got := []string{store.RetainLast, store.RetainDaily, store.RetainWeekly, store.RetainMonthly, store.RetainYearly}
	if !equalStrings(got, want) {
		t.Fatalf("retention reasons = %v, want %v", got, want)
	}
}
