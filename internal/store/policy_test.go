package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"proxback/internal/store"
)

// TestJobPolicyDefaults pins the promise the whole feature rests on: a job that
// never opened Advanced protection behaves exactly as a job did before the
// policy existed, and a partially specified policy keeps the defaults for
// everything it did not mention.
func TestJobPolicyDefaults(t *testing.T) {
	def := store.DefaultPolicy()
	if def.Quiesce != store.QuiesceNone || def.RetryCount != 0 || def.MaxDurationMinutes != 0 ||
		def.Window != nil || def.PreScript != "" || def.PostScript != "" ||
		def.UploadLimitMbpsOverride != 0 {
		t.Fatalf("default policy is not the inert one: %+v", def)
	}
	if def.RetryDelayMinutes != 5 || def.ScriptTimeoutSeconds != 30 {
		t.Fatalf("default policy delays = %+v, want 5 minutes and 30 seconds", def)
	}
	if !def.IsDefault() {
		t.Fatal("the default policy does not report itself as default")
	}

	// A one-field policy still times its scripts out after 30 seconds: an
	// absent field is not a zero.
	var partial store.JobPolicy
	if err := json.Unmarshal([]byte(`{"quiesce":"guest-agent"}`), &partial); err != nil {
		t.Fatalf("decode partial policy: %v", err)
	}
	if partial.ScriptTimeoutSeconds != 30 || partial.RetryDelayMinutes != 5 {
		t.Fatalf("partial policy = %+v, want the untouched defaults kept", partial)
	}
	if partial.IsDefault() {
		t.Fatal("a policy that asks for quiescing reports itself as default")
	}
	// Absent lists are empty, never nil, so they serialise as [].
	if partial.ExcludeDisks == nil || partial.ExcludePaths == nil {
		t.Fatalf("partial policy lists = %#v / %#v, want empty slices",
			partial.ExcludeDisks, partial.ExcludePaths)
	}

	// null and an absent policy both mean the defaults.
	var fromNull store.JobPolicy
	if err := json.Unmarshal([]byte(`null`), &fromNull); err != nil {
		t.Fatalf("decode null policy: %v", err)
	}
	if !fromNull.IsDefault() {
		t.Fatalf("null policy = %+v, want the defaults", fromNull)
	}
}

// TestJobPolicyValidate is the matrix the API's 400s come from. Every message
// has to name the field the console can highlight.
func TestJobPolicyValidate(t *testing.T) {
	base := func(mutate func(p *store.JobPolicy)) store.JobPolicy {
		p := store.DefaultPolicy()
		mutate(&p)
		return p
	}
	for _, c := range []struct {
		name      string
		kind      string
		policy    store.JobPolicy
		wantField string
	}{
		{"the defaults are valid for a vm job", store.SourceVM, store.DefaultPolicy(), ""},
		{"the defaults are valid for an agent job", store.SourceAgent, store.DefaultPolicy(), ""},
		{"a full policy", store.SourceVM, base(func(p *store.JobPolicy) {
			p.Quiesce = store.QuiesceGuestAgent
			p.ExcludeDisks = []string{"scsi1", "virtio2"}
			p.RetryCount = 5
			p.RetryDelayMinutes = 120
			p.MaxDurationMinutes = 240
			p.Window = &store.BackupWindow{Start: "22:00", End: "06:00"}
			p.PreScript = "/usr/local/bin/freeze.sh"
			p.PostScript = "/usr/local/bin/thaw.sh"
			p.ScriptTimeoutSeconds = 3600
			p.UploadLimitMbpsOverride = 500
		}), ""},

		{"an unknown quiesce mode", store.SourceVM, base(func(p *store.JobPolicy) {
			p.Quiesce = "vss"
		}), "policy.quiesce"},

		{"excluded disks on an agent job", store.SourceAgent, base(func(p *store.JobPolicy) {
			p.ExcludeDisks = []string{"scsi1"}
		}), "policy.excludeDisks"},
		{"a disk key that is not one", store.SourceVM, base(func(p *store.JobPolicy) {
			p.ExcludeDisks = []string{"/var/lib/vz"}
		}), "policy.excludeDisks"},
		{"a disk family that does not exist", store.SourceVM, base(func(p *store.JobPolicy) {
			p.ExcludeDisks = []string{"nvme0"}
		}), "policy.excludeDisks"},

		{"excluded paths on a vm job", store.SourceVM, base(func(p *store.JobPolicy) {
			p.ExcludePaths = []string{"**/node_modules"}
		}), "policy.excludePaths"},
		{"a malformed glob", store.SourceAgent, base(func(p *store.JobPolicy) {
			p.ExcludePaths = []string{"var/[log"}
		}), "policy.excludePaths"},

		{"too many retries", store.SourceVM, base(func(p *store.JobPolicy) {
			p.RetryCount = 6
		}), "policy.retryCount"},
		{"a negative retry count", store.SourceVM, base(func(p *store.JobPolicy) {
			p.RetryCount = -1
		}), "policy.retryCount"},
		{"a retry delay below the floor", store.SourceVM, base(func(p *store.JobPolicy) {
			p.RetryDelayMinutes = -5
		}), "policy.retryDelayMinutes"},
		{"a retry delay above the ceiling", store.SourceVM, base(func(p *store.JobPolicy) {
			p.RetryDelayMinutes = 121
		}), "policy.retryDelayMinutes"},

		{"a duration limit beyond a week", store.SourceVM, base(func(p *store.JobPolicy) {
			p.MaxDurationMinutes = 10081
		}), "policy.maxDurationMinutes"},
		{"a negative duration limit", store.SourceVM, base(func(p *store.JobPolicy) {
			p.MaxDurationMinutes = -1
		}), "policy.maxDurationMinutes"},

		{"a window with a broken start", store.SourceVM, base(func(p *store.JobPolicy) {
			p.Window = &store.BackupWindow{Start: "9:00", End: "17:00"}
		}), "policy.window.start"},
		{"a window with a broken end", store.SourceVM, base(func(p *store.JobPolicy) {
			p.Window = &store.BackupWindow{Start: "09:00", End: "25:00"}
		}), "policy.window.end"},
		{"a window of zero length", store.SourceVM, base(func(p *store.JobPolicy) {
			p.Window = &store.BackupWindow{Start: "09:00", End: "09:00"}
		}), "policy.window"},

		{"a script timeout of zero is the default, not an error", store.SourceVM, base(func(p *store.JobPolicy) {
			p.ScriptTimeoutSeconds = 0
		}), ""},
		{"a negative script timeout", store.SourceVM, base(func(p *store.JobPolicy) {
			p.ScriptTimeoutSeconds = -1
		}), "policy.scriptTimeoutSeconds"},
		{"a script timeout beyond an hour", store.SourceVM, base(func(p *store.JobPolicy) {
			p.ScriptTimeoutSeconds = 3601
		}), "policy.scriptTimeoutSeconds"},

		{"a negative transfer ceiling", store.SourceVM, base(func(p *store.JobPolicy) {
			p.UploadLimitMbpsOverride = -1
		}), "policy.uploadLimitMbpsOverride"},
		{"a transfer ceiling beyond the maximum", store.SourceVM, base(func(p *store.JobPolicy) {
			p.UploadLimitMbpsOverride = 10001
		}), "policy.uploadLimitMbpsOverride"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.policy.Validate(c.kind)
			if c.wantField == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %+v", c.policy)
			}
			if !strings.Contains(err.Error(), c.wantField) {
				t.Fatalf("Validate = %q, want it to name %s", err, c.wantField)
			}
		})
	}
}

// TestBackupWindowContains covers the case that makes windows worth testing at
// all: the ordinary night window crosses midnight, so "after it opens or before
// it closes" is the rule, not "between".
func TestBackupWindowContains(t *testing.T) {
	night := store.BackupWindow{Start: "22:00", End: "06:00"}
	day := store.BackupWindow{Start: "09:00", End: "17:00"}
	clock := func(h, m int) time.Time {
		return time.Date(2026, time.March, 1, h, m, 0, 0, time.UTC)
	}
	for _, c := range []struct {
		window store.BackupWindow
		hour   int
		minute int
		want   bool
	}{
		{night, 22, 0, true},   // opens exactly on the hour
		{night, 23, 30, true},  // before midnight
		{night, 0, 1, true},    // after midnight
		{night, 5, 59, true},   // the last minute inside
		{night, 6, 0, false},   // the close is exclusive
		{night, 12, 0, false},  // the middle of the day
		{night, 21, 59, false}, // one minute early
		{day, 9, 0, true},
		{day, 16, 59, true},
		{day, 17, 0, false},
		{day, 3, 0, false},
	} {
		if got := c.window.Contains(clock(c.hour, c.minute)); got != c.want {
			t.Errorf("%s contains %02d:%02d = %v, want %v", c.window, c.hour, c.minute, got, c.want)
		}
	}
	if night.String() != "22:00–06:00" {
		t.Fatalf("window renders as %q", night.String())
	}
}

func TestMatchGlob(t *testing.T) {
	for _, c := range []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/node_modules", "srv/app/node_modules", true},
		{"**/node_modules", "node_modules", true},
		{"**/node_modules", "srv/app/node_modules/react/index.js", true},
		{"**/node_modules", "srv/app/nodes", false},
		{"var/log/*.gz", "var/log/syslog.1.gz", true},
		{"var/log/*.gz", "var/log/nested/syslog.gz", false},
		{"var/log", "var/log/syslog", true},
		{"*.tmp", "scratch.tmp", true},
		{"*.tmp", "srv/scratch.tmp", false},
		{"**/*.tmp", "srv/deep/scratch.tmp", true},
		{"**", "anything/at/all", true},
	} {
		got, err := store.MatchGlob(c.pattern, c.path)
		if err != nil {
			t.Fatalf("MatchGlob(%q, %q): %v", c.pattern, c.path, err)
		}
		if got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
	if _, err := store.MatchGlob("var/[log", "var/log"); err == nil {
		t.Fatal("a malformed pattern was accepted")
	}
}

func TestJobPolicyNormalized(t *testing.T) {
	p := store.JobPolicy{
		Quiesce:      " Guest-Agent ",
		ExcludeDisks: []string{" SCSI1 ", "scsi1", "", "virtio0"},
		ExcludePaths: []string{" **/tmp ", "**/tmp", ""},
		PreScript:    "  /bin/freeze  ",
	}.Normalized()
	if p.Quiesce != store.QuiesceGuestAgent {
		t.Fatalf("quiesce normalised to %q", p.Quiesce)
	}
	if len(p.ExcludeDisks) != 2 || p.ExcludeDisks[0] != "scsi1" || p.ExcludeDisks[1] != "virtio0" {
		t.Fatalf("excludeDisks normalised to %#v", p.ExcludeDisks)
	}
	if len(p.ExcludePaths) != 1 || p.ExcludePaths[0] != "**/tmp" {
		t.Fatalf("excludePaths normalised to %#v", p.ExcludePaths)
	}
	if p.PreScript != "/bin/freeze" {
		t.Fatalf("preScript normalised to %q", p.PreScript)
	}
	// Path patterns keep their case: file systems that care about it exist.
	q := store.JobPolicy{ExcludePaths: []string{"**/Node_Modules"}}.Normalized()
	if q.ExcludePaths[0] != "**/Node_Modules" {
		t.Fatalf("excludePaths lost its case: %q", q.ExcludePaths[0])
	}
}

func TestValidDiskKey(t *testing.T) {
	for _, key := range []string{"scsi0", "scsi15", "virtio1", "ide2", "sata3", "efidisk0", "tpmstate0", "unused0"} {
		if !store.ValidDiskKey(key) {
			t.Errorf("ValidDiskKey(%q) = false", key)
		}
	}
	for _, key := range []string{"", "scsi", "scsi1a", "nvme0", "/dev/sda", "disk0"} {
		if store.ValidDiskKey(key) {
			t.Errorf("ValidDiskKey(%q) = true", key)
		}
	}
}
