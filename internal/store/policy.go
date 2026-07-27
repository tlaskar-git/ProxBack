package store

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// Quiesce modes. "none" reads the disk as it stands — a restore point that
// recovers the way a guest recovers from a power cut. "guest-agent" asks for the
// guest's filesystem to be frozen first, which needs qemu-guest-agent running
// inside the guest; ProxBack never claims a freeze it did not get.
const (
	QuiesceNone       = "none"
	QuiesceGuestAgent = "guest-agent"
)

// Bounds of the protection policy, enforced on POST/PATCH /api/jobs and again
// when a stored policy is read back. They are the same bounds the console
// enforces in its inputs, so a value the UI can produce is always accepted.
const (
	MinRetryCount        = 0
	MaxRetryCount        = 5
	MinRetryDelayMinutes = 1
	MaxRetryDelayMinutes = 120
	// MaxRunDurationMinutes is a week: past that the limit says nothing that
	// "no limit" does not already say.
	MaxRunDurationMinutes = 10080
	MinScriptTimeoutSecs  = 1
	MaxScriptTimeoutSecs  = 3600
)

// Policy defaults. They are what a job created without an Advanced protection
// step behaves like, so every one of them must be the safe choice.
const (
	DefaultRetryDelayMinutes = 5
	DefaultScriptTimeoutSecs = 30
)

// BackupWindow is the wall-clock window a run may *start* inside, in the
// server's local timezone. A run already under way is never cut off when the
// window closes: the window governs starting, not running.
type BackupWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Validate reports why a window cannot be used. The API turns a non-nil result
// into a 400 naming the field.
func (w BackupWindow) Validate() error {
	if !validClockTime(w.Start) {
		return fmt.Errorf(`policy.window.start must be "HH:MM" in 24 hour form, got %q`, w.Start)
	}
	if !validClockTime(w.End) {
		return fmt.Errorf(`policy.window.end must be "HH:MM" in 24 hour form, got %q`, w.End)
	}
	if w.Start == w.End {
		return fmt.Errorf("policy.window.start and policy.window.end must differ; %q to %q is not a window", w.Start, w.End)
	}
	return nil
}

// validClockTime reports whether v is a real "HH:MM" wall-clock time.
func validClockTime(v string) bool {
	h, m, ok := parseClockTime(v)
	return ok && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// clockMinutes renders "HH:MM" as minutes past midnight.
func clockMinutes(v string) (int, bool) {
	if !validClockTime(v) {
		return 0, false
	}
	h, m, _ := parseClockTime(v)
	return h*60 + m, true
}

// Contains reports whether t's wall clock falls inside the window. The end is
// exclusive, and a window whose end is before its start crosses midnight —
// 22:00–06:00 is the ordinary night window and must include 23:30 and 02:00.
func (w BackupWindow) Contains(t time.Time) bool {
	start, ok := clockMinutes(w.Start)
	if !ok {
		return true // an unusable window never blocks a run
	}
	end, ok := clockMinutes(w.End)
	if !ok {
		return true
	}
	now := t.Hour()*60 + t.Minute()
	if start == end {
		return true
	}
	if start < end {
		return now >= start && now < end
	}
	// Crosses midnight: inside means "after it opens, or before it closes".
	return now >= start || now < end
}

// String renders the window the way the run log names it.
func (w BackupWindow) String() string { return w.Start + "–" + w.End }

// JobPolicy is a job's optional protection policy. Every field is optional on
// the wire and every default keeps the simple case simple: a job created
// without touching Advanced protection behaves exactly as it did before the
// policy existed.
type JobPolicy struct {
	// Quiesce is QuiesceNone or QuiesceGuestAgent.
	Quiesce string `json:"quiesce"`
	// ExcludeDisks names Proxmox disk keys (scsi1, virtio2 …) a vm job leaves
	// out. It is only honoured on the per-disk export path; see
	// sched.ErrExcludeDisksUnsupported for what happens on the vzdump path.
	ExcludeDisks []string `json:"excludeDisks"`
	// ExcludePaths are glob patterns an agent job's file walk skips.
	ExcludePaths []string `json:"excludePaths"`
	// RetryCount is how many extra attempts a failed run gets (0–5), spaced
	// RetryDelayMinutes apart.
	RetryCount        int `json:"retryCount"`
	RetryDelayMinutes int `json:"retryDelayMinutes"`
	// MaxDurationMinutes cancels a run that outlives it; 0 is no limit.
	MaxDurationMinutes int `json:"maxDurationMinutes"`
	// Window is the wall-clock window a scheduled run may start inside; nil
	// means any time.
	Window *BackupWindow `json:"window"`
	// PreScript and PostScript run where the data lives — on the node helper for
	// vm jobs, on the agent for agent jobs — with their combined output captured
	// into the run log.
	PreScript  string `json:"preScript"`
	PostScript string `json:"postScript"`
	// ScriptTimeoutSeconds bounds each script; the process is killed on expiry.
	ScriptTimeoutSeconds int `json:"scriptTimeoutSeconds"`
	// UploadLimitMbpsOverride replaces the global upload ceiling for this job's
	// runs; 0 inherits the global setting.
	UploadLimitMbpsOverride int `json:"uploadLimitMbpsOverride"`
}

// DefaultPolicy is the policy a job without one behaves like.
func DefaultPolicy() JobPolicy {
	return JobPolicy{
		Quiesce:              QuiesceNone,
		ExcludeDisks:         []string{},
		ExcludePaths:         []string{},
		RetryDelayMinutes:    DefaultRetryDelayMinutes,
		ScriptTimeoutSeconds: DefaultScriptTimeoutSecs,
	}
}

// policyJSON is the decoding shape. Every field is a pointer so an absent field
// keeps its default rather than collapsing to a zero that means something else:
// a policy sent as {"quiesce":"guest-agent"} must still time its scripts out
// after 30 seconds, not immediately.
type policyJSON struct {
	Quiesce                 *string       `json:"quiesce"`
	ExcludeDisks            []string      `json:"excludeDisks"`
	ExcludePaths            []string      `json:"excludePaths"`
	RetryCount              *int          `json:"retryCount"`
	RetryDelayMinutes       *int          `json:"retryDelayMinutes"`
	MaxDurationMinutes      *int          `json:"maxDurationMinutes"`
	Window                  *BackupWindow `json:"window"`
	PreScript               *string       `json:"preScript"`
	PostScript              *string       `json:"postScript"`
	ScriptTimeoutSeconds    *int          `json:"scriptTimeoutSeconds"`
	UploadLimitMbpsOverride *int          `json:"uploadLimitMbpsOverride"`
}

// UnmarshalJSON fills the gaps of a partial policy with the defaults, so both a
// full object from the console and a one-field object from a script produce a
// complete, meaningful policy.
func (p *JobPolicy) UnmarshalJSON(b []byte) error {
	trimmed := trimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*p = DefaultPolicy()
		return nil
	}
	var in policyJSON
	if err := json.Unmarshal(trimmed, &in); err != nil {
		return fmt.Errorf("decode job policy: %w", err)
	}
	out := DefaultPolicy()
	if in.Quiesce != nil {
		out.Quiesce = strings.ToLower(strings.TrimSpace(*in.Quiesce))
	}
	if in.ExcludeDisks != nil {
		out.ExcludeDisks = in.ExcludeDisks
	}
	if in.ExcludePaths != nil {
		out.ExcludePaths = in.ExcludePaths
	}
	if in.RetryCount != nil {
		out.RetryCount = *in.RetryCount
	}
	if in.RetryDelayMinutes != nil {
		out.RetryDelayMinutes = *in.RetryDelayMinutes
	}
	if in.MaxDurationMinutes != nil {
		out.MaxDurationMinutes = *in.MaxDurationMinutes
	}
	if in.Window != nil {
		w := *in.Window
		out.Window = &w
	}
	if in.PreScript != nil {
		out.PreScript = *in.PreScript
	}
	if in.PostScript != nil {
		out.PostScript = *in.PostScript
	}
	if in.ScriptTimeoutSeconds != nil {
		out.ScriptTimeoutSeconds = *in.ScriptTimeoutSeconds
	}
	if in.UploadLimitMbpsOverride != nil {
		out.UploadLimitMbpsOverride = *in.UploadLimitMbpsOverride
	}
	*p = out.Normalized()
	return nil
}

// Normalized tidies a policy: strings trimmed, lists de-duplicated and never
// nil, empty defaults restored. Out-of-range numbers are left alone so
// validation can complain about them by name.
func (p JobPolicy) Normalized() JobPolicy {
	p.Quiesce = strings.ToLower(strings.TrimSpace(p.Quiesce))
	if p.Quiesce == "" {
		p.Quiesce = QuiesceNone
	}
	p.ExcludeDisks = normalizeList(p.ExcludeDisks, strings.ToLower)
	p.ExcludePaths = normalizeList(p.ExcludePaths, nil)
	p.PreScript = strings.TrimSpace(p.PreScript)
	p.PostScript = strings.TrimSpace(p.PostScript)
	if p.RetryDelayMinutes == 0 {
		p.RetryDelayMinutes = DefaultRetryDelayMinutes
	}
	if p.ScriptTimeoutSeconds == 0 {
		p.ScriptTimeoutSeconds = DefaultScriptTimeoutSecs
	}
	if p.Window != nil {
		w := BackupWindow{
			Start: strings.TrimSpace(p.Window.Start),
			End:   strings.TrimSpace(p.Window.End),
		}
		if w.Start == "" && w.End == "" {
			p.Window = nil
		} else {
			p.Window = &w
		}
	}
	return p
}

// normalizeList trims, drops empties and de-duplicates while preserving the
// operator's order — an exclusion list reads back the way it was typed.
func normalizeList(in []string, fold func(string) string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		item := strings.TrimSpace(v)
		if fold != nil {
			item = fold(item)
		}
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// IsDefault reports whether nothing in the policy departs from the defaults,
// which is what lets the UI say "standard protection" instead of reciting ten
// default values.
func (p JobPolicy) IsDefault() bool {
	p = p.Normalized()
	d := DefaultPolicy()
	return p.Quiesce == d.Quiesce &&
		len(p.ExcludeDisks) == 0 && len(p.ExcludePaths) == 0 &&
		p.RetryCount == d.RetryCount && p.RetryDelayMinutes == d.RetryDelayMinutes &&
		p.MaxDurationMinutes == d.MaxDurationMinutes && p.Window == nil &&
		p.PreScript == "" && p.PostScript == "" &&
		p.ScriptTimeoutSeconds == d.ScriptTimeoutSeconds &&
		p.UploadLimitMbpsOverride == d.UploadLimitMbpsOverride
}

// ScriptTimeoutSecondsOrDefault returns the script timeout, substituting the
// default for an unset one.
func (p JobPolicy) ScriptTimeoutSecondsOrDefault() int {
	if p.ScriptTimeoutSeconds <= 0 {
		return DefaultScriptTimeoutSecs
	}
	return p.ScriptTimeoutSeconds
}

// HasScripts reports whether the policy asks for anything to be executed.
func (p JobPolicy) HasScripts() bool { return p.PreScript != "" || p.PostScript != "" }

// diskKeyPrefixes are the Proxmox disk key families a vm job can exclude. The
// list is what the operator sees in the guest's hardware tab; anything else is
// refused rather than silently ignored, because an exclusion nobody applies is
// the most dangerous kind of setting there is.
var diskKeyPrefixes = []string{"scsi", "virtio", "ide", "sata", "efidisk", "tpmstate", "unused"}

// ValidDiskKey reports whether v names a Proxmox disk, e.g. "scsi1".
func ValidDiskKey(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, prefix := range diskKeyPrefixes {
		if !strings.HasPrefix(v, prefix) {
			continue
		}
		return allDigits(v[len(prefix):])
	}
	return false
}

// Validate reports why a policy cannot be used, naming the offending field the
// way the API's 400 must. kind is the job's kind, because two of the fields
// only mean anything for one of them.
func (p JobPolicy) Validate(kind string) error {
	p = p.Normalized()
	switch p.Quiesce {
	case QuiesceNone, QuiesceGuestAgent:
	default:
		return fmt.Errorf(`policy.quiesce must be "none" or "guest-agent", got %q`, p.Quiesce)
	}
	if len(p.ExcludeDisks) > 0 {
		if kind != SourceVM {
			return fmt.Errorf("policy.excludeDisks only applies to vm jobs")
		}
		for _, d := range p.ExcludeDisks {
			if !ValidDiskKey(d) {
				return fmt.Errorf("policy.excludeDisks: %q is not a Proxmox disk key (expected e.g. scsi1, virtio0, sata2)", d)
			}
		}
	}
	if len(p.ExcludePaths) > 0 && kind != SourceAgent {
		return fmt.Errorf("policy.excludePaths only applies to agent jobs")
	}
	for _, pattern := range p.ExcludePaths {
		if err := validateGlob(pattern); err != nil {
			return fmt.Errorf("policy.excludePaths: %w", err)
		}
	}
	if p.RetryCount < MinRetryCount || p.RetryCount > MaxRetryCount {
		return fmt.Errorf("policy.retryCount must be between %d and %d, got %d",
			MinRetryCount, MaxRetryCount, p.RetryCount)
	}
	if p.RetryDelayMinutes < MinRetryDelayMinutes || p.RetryDelayMinutes > MaxRetryDelayMinutes {
		return fmt.Errorf("policy.retryDelayMinutes must be between %d and %d, got %d",
			MinRetryDelayMinutes, MaxRetryDelayMinutes, p.RetryDelayMinutes)
	}
	if p.MaxDurationMinutes < 0 || p.MaxDurationMinutes > MaxRunDurationMinutes {
		return fmt.Errorf("policy.maxDurationMinutes must be between 0 and %d, got %d",
			MaxRunDurationMinutes, p.MaxDurationMinutes)
	}
	if p.Window != nil {
		if err := p.Window.Validate(); err != nil {
			return err
		}
	}
	if p.ScriptTimeoutSeconds < MinScriptTimeoutSecs || p.ScriptTimeoutSeconds > MaxScriptTimeoutSecs {
		return fmt.Errorf("policy.scriptTimeoutSeconds must be between %d and %d, got %d",
			MinScriptTimeoutSecs, MaxScriptTimeoutSecs, p.ScriptTimeoutSeconds)
	}
	if p.UploadLimitMbpsOverride < MinUploadLimitMbps || p.UploadLimitMbpsOverride > MaxUploadLimitMbps {
		return fmt.Errorf("policy.uploadLimitMbpsOverride must be between %d and %d, got %d",
			MinUploadLimitMbps, MaxUploadLimitMbps, p.UploadLimitMbpsOverride)
	}
	return nil
}

// validateGlob rejects a pattern the file walk could never apply. The syntax is
// path/filepath.Match's, extended with "**" for "any number of segments", which
// is how the console phrases its examples.
func validateGlob(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("a pattern must not be empty")
	}
	// Every segment is checked, not just the ones a probe path happens to
	// reach: a pattern is rejected for being malformed anywhere in it.
	for _, segment := range strings.Split(strings.ReplaceAll(pattern, `\`, "/"), "/") {
		if segment == "**" || segment == "" {
			continue
		}
		if _, err := path.Match(segment, "probe"); err != nil {
			return fmt.Errorf("%q is not a valid pattern: %w", pattern, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- glob syntax

// MatchGlob reports whether a slash-separated path matches an exclusion
// pattern. The syntax is path/filepath.Match's per segment, plus "**" for "any
// number of segments, including none" — so "**/node_modules" excludes
// node_modules wherever it appears and "var/log/*.gz" only there.
//
// It is defined here rather than in the walker because it is the contract: the
// server validates patterns against it and the agent applies it, and the two
// must be the same rule.
func MatchGlob(pattern, path string) (bool, error) {
	pattern = strings.Trim(strings.ReplaceAll(pattern, `\`, "/"), "/")
	path = strings.Trim(strings.ReplaceAll(path, `\`, "/"), "/")
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

// matchSegments matches pattern segments against path segments, treating "**"
// as a wildcard over whole segments.
func matchSegments(pattern, path []string) (bool, error) {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			if len(rest) == 0 {
				return true, nil
			}
			for i := 0; i <= len(path); i++ {
				ok, err := matchSegments(rest, path[i:])
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		}
		if len(path) == 0 {
			return false, nil
		}
		ok, err := pathMatch(pattern[0], path[0])
		if err != nil || !ok {
			return false, err
		}
		pattern, path = pattern[1:], path[1:]
	}
	// A pattern that names a directory excludes everything under it.
	return true, nil
}

// pathMatch is filepath.Match restricted to one segment, with the platform's
// separator taken out of the picture.
func pathMatch(pattern, name string) (bool, error) {
	ok, err := path.Match(pattern, name)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// ---------------------------------------------------------------- persistence

// encodePolicy renders a policy for the jobs.policy column. A policy that is
// entirely default is stored as the empty string, so a database written by an
// operator who never opened Advanced protection stays free of noise and
// existing rows (NULL/empty) already mean exactly the same thing.
func encodePolicy(p JobPolicy) (string, error) {
	p = p.Normalized()
	if p.IsDefault() {
		return "", nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode job policy: %w", err)
	}
	return string(raw), nil
}

// decodePolicy reads the jobs.policy column. Empty (which is what every row
// written before the column existed holds) means the defaults.
func decodePolicy(raw string) JobPolicy {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultPolicy()
	}
	var p JobPolicy
	if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
		// An unreadable policy must not make a job unlistable; the defaults are
		// the safe reading, and validation refuses to write one back.
		return DefaultPolicy()
	}
	return p.Normalized()
}
