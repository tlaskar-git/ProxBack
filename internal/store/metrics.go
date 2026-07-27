package store

import "fmt"

// Data-reduction metrics, defined once for the whole product.
//
// Two numbers describe the same fact and they must never disagree:
//
//   - ReductionPct is how much of what was read did not have to travel,
//     as a percentage: (1 - uploaded/processed) * 100. It is 0 when nothing was
//     processed, and it is what an operator reads as "100% avoided".
//   - ReductionRatio is processed/uploaded, the "4.0×" form. It does not exist
//     when nothing was uploaded — the ratio is unbounded there — so it is
//     reported as absent rather than as some placeholder. A run that read
//     32 MiB and uploaded nothing is 100% avoided, never 1.0×.
//
// Every surface (the runs API, the run log, notifications) reads these
// functions rather than doing its own arithmetic.

// ReductionPct is the percentage of processed bytes that did not need
// uploading, clamped to 0–100. It is 0 when nothing was processed.
func ReductionPct(processed, uploaded int64) float64 {
	if processed <= 0 {
		return 0
	}
	pct := (1 - float64(uploaded)/float64(processed)) * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// ReductionRatio is processed/uploaded — the "×" form of the same fact. The
// second result is false when the ratio does not exist: nothing was uploaded
// (unbounded) or nothing was processed (undefined).
func ReductionRatio(processed, uploaded int64) (float64, bool) {
	if processed <= 0 || uploaded <= 0 {
		return 0, false
	}
	return float64(processed) / float64(uploaded), true
}

// ReductionSummary renders the one phrase the run log and any other prose
// surface uses, so the wording can never contradict the numbers:
//
//	"100% avoided (nothing needed uploading)"
//	"75% avoided (4.0× reduction)"
//	"0% avoided"
func ReductionSummary(processed, uploaded int64) string {
	pct := ReductionPct(processed, uploaded)
	ratio, ok := ReductionRatio(processed, uploaded)
	if !ok {
		if processed > 0 && uploaded <= 0 {
			return "100% avoided (nothing needed uploading)"
		}
		return fmt.Sprintf("%.0f%% avoided", pct)
	}
	if ratio < 1.05 {
		// Anything at or below ~1× is not a reduction worth naming.
		return fmt.Sprintf("%.0f%% avoided", pct)
	}
	return fmt.Sprintf("%.0f%% avoided (%.1f× reduction)", pct, ratio)
}
