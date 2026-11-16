package sched

import (
	"fmt"
	"strings"
	"time"

	"proxback/internal/engine"
)

// SourceFailure is one guest that a VM job could not back up.
type SourceFailure struct {
	Name string
	Err  error
}

// PartialError is the outcome of a VM job in which some guests were backed up
// and some were not. The run is recorded as partial, not failed: the guests
// that succeeded have real restore points, and the run's error names every
// guest that did not, with its own error.
type PartialError struct {
	// Total is the number of guests in the job.
	Total    int
	Failures []SourceFailure
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("%d of %d VMs failed: %s", len(e.Failures), e.Total, failureList(e.Failures))
}

// Unwrap exposes the per-guest errors to errors.Is and errors.As.
func (e *PartialError) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		out = append(out, f.Err)
	}
	return out
}

// AllFailedError is the outcome of a VM job with more than one guest in which
// every guest failed. A single-guest job reports that guest's own error.
type AllFailedError struct {
	Failures []SourceFailure
}

func (e *AllFailedError) Error() string {
	return fmt.Sprintf("all %d VMs failed: %s", len(e.Failures), failureList(e.Failures))
}

// Unwrap exposes the per-guest errors to errors.Is and errors.As.
func (e *AllFailedError) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		out = append(out, f.Err)
	}
	return out
}

func failureList(failures []SourceFailure) string {
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, f.Name+": "+f.Err.Error())
	}
	return strings.Join(parts, "; ")
}

// vmJobState is what a VM job remembers between the attempts of one run: which
// monitor row each guest owns, which guests are already backed up, and the
// bytes the earlier attempts moved.
type vmJobState struct {
	planned bool
	order   []string
	names   map[string]string
	seq     map[string]int
	done    map[string]bool
	carried engine.Stats
}

func newVMJobState() *vmJobState {
	return &vmJobState{
		names: map[string]string{},
		seq:   map[string]int{},
		done:  map[string]bool{},
	}
}

// record fixes the run's guest list from the first plan.
func (s *vmJobState) record(plans []vmPlan) {
	s.planned = true
	for i, p := range plans {
		s.order = append(s.order, p.sourceID)
		s.names[p.sourceID] = p.name
		s.seq[p.sourceID] = i
	}
}

// remaining keeps the guests of the first plan that are not backed up yet.
func (s *vmJobState) remaining(plans []vmPlan) []vmPlan {
	out := make([]vmPlan, 0, len(plans))
	for _, p := range plans {
		if _, known := s.seq[p.sourceID]; known && !s.done[p.sourceID] {
			out = append(out, p)
		}
	}
	return out
}

// stats adds the bytes of earlier attempts to the current session's figures, so
// the run reports everything it moved, not only its last attempt.
func (s *vmJobState) stats(sess *engine.Session) *engine.Stats {
	cur := sess.Stats()
	cur.BytesProcessed += s.carried.BytesProcessed
	cur.BytesUploaded += s.carried.BytesUploaded
	cur.BytesDeduped += s.carried.BytesDeduped
	cur.ChunksTotal += s.carried.ChunksTotal
	cur.ChunksUploaded += s.carried.ChunksUploaded
	cur.ChunksDeduped += s.carried.ChunksDeduped
	return &cur
}

// carry stores an attempt's cumulative totals for the next attempt.
func (s *vmJobState) carry(total engine.Stats) {
	s.carried = total
}

// outcome turns an attempt's failures into the run's result.
func (s *vmJobState) outcome(failures []SourceFailure) error {
	if len(failures) == 0 {
		return nil
	}
	if len(s.done) > 0 {
		return &PartialError{Total: len(s.order), Failures: failures}
	}
	if len(failures) == 1 && len(s.order) == 1 {
		return failures[0].Err
	}
	return &AllFailedError{Failures: failures}
}

// partialGCNote is the run-log line of a partial run that still collects
// orphan chunks: it says what keeps the failed guests' uploads safe.
func partialGCNote(failed int) string {
	g := gcGrace()
	if g <= 0 {
		return fmt.Sprintf("garbage collection runs although %s failed: the grace window is disabled, "+
			"so chunks the failed VMs uploaded are collected", countNoun(failed, "VM"))
	}
	window := g.String()
	if g%time.Hour == 0 {
		window = fmt.Sprintf("%dh", int(g/time.Hour))
	}
	return fmt.Sprintf("garbage collection runs although %s failed: chunks uploaded in the last %s "+
		"are protected by the grace window", countNoun(failed, "VM"), window)
}
