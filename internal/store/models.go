package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TagSeparator joins tags in the persisted `tags` column.
const TagSeparator = ";"

// encodeTags renders a tag slice for storage.
func encodeTags(tags []string) string { return strings.Join(NormalizeTags(tags), TagSeparator) }

// decodeTags parses a persisted tag column back into a slice. It accepts both
// separators Proxmox tolerates so databases written by any version load.
func decodeTags(raw string) []string {
	return NormalizeTags(strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == ','
	}))
}

// NormalizeTags lower-cases, trims, de-duplicates and sorts tags. The result is
// never nil so it serialises as an empty JSON array rather than null.
func NormalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		tag := strings.ToLower(strings.TrimSpace(t))
		if tag == "" {
			continue
		}
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// NewID returns a random 32 hex character identifier.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never fails on supported platforms.
		panic(fmt.Sprintf("proxback: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// User is a control-panel operator.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"-"`
}

// Session is an opaque server-side session token.
type Session struct {
	Token     string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// PVEHost is a configured Proxmox VE endpoint. TokenSecret is only populated by
// accessors that explicitly decrypt it.
type PVEHost struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	BaseURL     string     `json:"baseUrl"`
	TokenID     string     `json:"tokenId"`
	TokenSecret string     `json:"-"`
	InsecureTLS bool       `json:"insecureTLS"`
	Status      string     `json:"status"`
	LastSeen    *time.Time `json:"lastSeen"`
	CreatedAt   time.Time  `json:"-"`
}

// VM is a cached or live Proxmox guest.
type VM struct {
	VMID     int      `json:"vmid"`
	Name     string   `json:"name"`
	Node     string   `json:"node"`
	Status   string   `json:"status"`
	MaxDisk  int64    `json:"maxdisk"`
	MaxMem   int64    `json:"maxmem"`
	Uptime   int64    `json:"uptime"`
	Tags     []string `json:"tags"`
	HostID   string   `json:"hostId,omitempty"`
	HostName string   `json:"hostName,omitempty"`
}

// HasTag reports whether the guest carries tag. Comparison is case insensitive
// because both sides are normalised to lower case on the way in.
func (v VM) HasTag(tag string) bool {
	want := strings.ToLower(strings.TrimSpace(tag))
	if want == "" {
		return false
	}
	for _, t := range v.Tags {
		if t == want {
			return true
		}
	}
	return false
}

// S3Target is a backup destination. SecretKey is only populated by accessors that
// explicitly decrypt it and is never serialised.
type S3Target struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Endpoint  string    `json:"endpoint"`
	Region    string    `json:"region"`
	Bucket    string    `json:"bucket"`
	AccessKey string    `json:"-"`
	SecretKey string    `json:"-"`
	PathStyle bool      `json:"pathStyle"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"-"`
}

// Agent is an enrolled in-guest agent.
type Agent struct {
	ID           string     `json:"id"`
	Hostname     string     `json:"hostname"`
	OS           string     `json:"os"`
	Arch         string     `json:"arch"`
	Version      string     `json:"version"`
	APIKeyHash   string     `json:"-"`
	LastSeen     *time.Time `json:"lastSeen"`
	RegisteredAt time.Time  `json:"registeredAt"`
}

// EnrollToken is a single-use enrollment token. Purpose is "agent" or "helper".
// A helper token also carries the Proxmox host it was minted for, which is what
// lets a node helper inherit its cluster identity at registration: the node
// itself never has to know which cluster it belongs to.
type EnrollToken struct {
	Token     string     `json:"token"`
	Purpose   string     `json:"-"`
	HostID    string     `json:"-"`
	CreatedAt time.Time  `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"-"`
}

// DefaultHelperPort is the port a node helper listens on unless told otherwise.
const DefaultHelperPort = 8007

// HelperUnassigned is the status of a node helper that is not bound to a
// Proxmox host. Registrations written before helpers carried a host identity
// migrate into it. Such a helper is never used to route backup or restore
// traffic: two clusters can each contain a node called "pve1", so a bare node
// name cannot say which physical machine is meant.
const HelperUnassigned = "unassigned"

// NodeHelper is an enrolled ProxBack node helper: the root daemon on a Proxmox
// node that wraps vzdump/qmrestore for agentless VM image backup. AccessSecret
// is the credential the server presents to the helper and is only populated by
// accessors that decrypt it; it is never serialised.
//
// A helper is identified by (HostID, Node): the node name alone is ambiguous
// across clusters. An empty HostID means the registration predates host
// identity (or has been unbound) and must not be used for routing.
type NodeHelper struct {
	ID string `json:"id"`
	// HostID is the Proxmox host (cluster) the helper's node belongs to. Empty
	// means unassigned.
	HostID       string     `json:"hostId"`
	Node         string     `json:"node"`
	Address      string     `json:"address"`
	Port         int        `json:"port"`
	Version      string     `json:"version"`
	AccessSecret string     `json:"-"`
	APIKeyHash   string     `json:"-"`
	LastSeen     *time.Time `json:"lastSeen"`
	RegisteredAt time.Time  `json:"registeredAt"`
}

// JobSource describes one backup source. VM jobs use HostID/VMID/Name; agent jobs
// use AgentID/Paths.
type JobSource struct {
	HostID  string   `json:"hostId,omitempty"`
	VMID    int      `json:"vmid,omitempty"`
	Name    string   `json:"name,omitempty"`
	AgentID string   `json:"agentId,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

// JobSources is always marshalled as a JSON array but accepts a bare object on
// input (the plan describes agent job sources as a single object).
type JobSources []JobSource

// UnmarshalJSON accepts either a JSON array of sources or a single source object.
func (js *JobSources) UnmarshalJSON(b []byte) error {
	trimmed := trimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*js = nil
		return nil
	}
	if trimmed[0] == '{' {
		var one JobSource
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return fmt.Errorf("decode job source object: %w", err)
		}
		*js = JobSources{one}
		return nil
	}
	var many []JobSource
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return fmt.Errorf("decode job sources: %w", err)
	}
	*js = many
	return nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}

// Job is a backup job definition.
type Job struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // vm | agent
	TargetID string `json:"targetId"`
	// Schedule is the structured schedule; the cron expression the scheduler
	// runs on is derived from it by Schedule.Cron().
	Schedule Schedule `json:"schedule"`
	// Retention is the GFS policy. A bare integer is still accepted on the wire
	// and means keep-last-N.
	Retention RetentionPolicy `json:"retention"`
	// Policy is the optional protection policy. A job that never opened the
	// Advanced protection step carries the defaults, which behave exactly as a
	// job did before the policy existed.
	Policy  JobPolicy  `json:"policy"`
	Enabled bool       `json:"enabled"`
	Sources JobSources `json:"sources"`
	// TagFilter makes a vm job's membership dynamic: at run start it resolves to
	// every cached guest carrying the tag, and Sources may then be empty. Empty
	// means the job uses its static Sources.
	TagFilter string    `json:"tagFilter"`
	CreatedAt time.Time `json:"-"`
}

// Run statuses.
const (
	RunRunning  = "running"
	RunSuccess  = "success"
	RunFailed   = "failed"
	RunCanceled = "canceled"
)

// Run kinds.
const (
	RunKindBackup  = "backup"
	RunKindRestore = "restore"
	RunKindVerify  = "verify"
)

// RestoreMeta is where a restore run put (or was asked to put) its data. It is
// persisted with the run so history can answer "where did this VM go?" without
// reading a log line.
type RestoreMeta struct {
	// Mode is RestoreAlongside or RestoreOverwrite.
	Mode     string `json:"mode"`
	HostID   string `json:"hostId,omitempty"`
	HostName string `json:"hostName,omitempty"`
	Node     string `json:"node,omitempty"`
	VMID     int    `json:"vmid,omitempty"`
	Storage  string `json:"storage,omitempty"`
	AgentID  string `json:"agentId,omitempty"`
	DestPath string `json:"destPath,omitempty"`
}

// Restore modes. Overwrite is never the default: a restore that lands on an
// existing guest destroys it.
const (
	RestoreAlongside = "alongside"
	RestoreOverwrite = "overwrite"
)

// ValidRestoreMode reports whether v names a supported restore mode.
func ValidRestoreMode(v string) bool {
	return v == RestoreAlongside || v == RestoreOverwrite
}

// JobRun is one execution of a job (or a restore operation).
type JobRun struct {
	ID             string     `json:"id"`
	JobID          string     `json:"jobId"`
	JobName        string     `json:"jobName"`
	Kind           string     `json:"-"`
	Status         string     `json:"status"`
	StartedAt      time.Time  `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	BytesProcessed int64      `json:"bytesProcessed"`
	BytesUploaded  int64      `json:"bytesUploaded"`
	// DedupRatio is the reduction expressed as a fraction (0–1). It is kept for
	// wire compatibility and is always ReductionPct/100 — the same source of
	// truth, never an independently computed number.
	DedupRatio float64 `json:"dedupRatio"`
	// ReductionPct is the percentage of processed bytes that did not have to be
	// uploaded, 0–100.
	ReductionPct float64 `json:"reductionPct"`
	// ReductionRatio is processed/uploaded, e.g. 4.0 for "4× reduction". It is
	// absent when nothing was uploaded, because the ratio is then unbounded —
	// which is exactly the case that used to be displayed as a nonsensical 1.0×.
	ReductionRatio *float64 `json:"reductionRatio,omitempty"`
	Error          string   `json:"error,omitempty"`
	ProgressPct    float64  `json:"progressPct"`
	CurrentStep    string   `json:"currentStep"`
	// Restore is the persisted destination of a restore run; nil for every other
	// kind of run.
	Restore *RestoreMeta `json:"restore,omitempty"`
}

// applyReductionMetrics derives the data-reduction fields from the run's byte
// counters. It is the single definition every reader sees: the API, the run
// log and the dashboard all read numbers produced here rather than computing
// their own, which is what stops a run from being 1.0× and 100% at once.
//
// Restores and verifications read only, so deduplication is meaningless for
// them and all three fields stay zero/absent.
func (r *JobRun) applyReductionMetrics() {
	if r.Kind == RunKindRestore || r.Kind == RunKindVerify {
		r.DedupRatio, r.ReductionPct, r.ReductionRatio = 0, 0, nil
		return
	}
	r.ReductionPct = ReductionPct(r.BytesProcessed, r.BytesUploaded)
	r.DedupRatio = r.ReductionPct / 100
	if ratio, ok := ReductionRatio(r.BytesProcessed, r.BytesUploaded); ok {
		r.ReductionRatio = &ratio
	} else {
		r.ReductionRatio = nil
	}
}

// RunSource is one object a run walks — a VM for an agentless job, an agent for
// a file-level one. The rows are written as the run progresses, so a run of 8
// VMs shows 8 rows advancing independently and the monitor can draw them.
type RunSource struct {
	// RunID is the owning run; it is not part of the API shape, which always
	// appears nested inside its run.
	RunID string `json:"-"`
	// Seq is the source's position in the run, starting at 0.
	Seq  int    `json:"seq"`
	Name string `json:"name"`
	Kind string `json:"kind"` // vm | agent
	// SourceID identifies the workload the row backed up: "<hostId>_<vmid>" for
	// a guest, the agent id for a file backup. It is what makes a run source
	// attributable to one workload when two clusters hold identically named VMs.
	SourceID string `json:"sourceId,omitempty"`
	// HostID and HostName name the cluster the guest lives in, so a row reads
	// "cluster / name (vmid) / node" rather than just a node name.
	HostID         string     `json:"hostId,omitempty"`
	HostName       string     `json:"hostName,omitempty"`
	Node           string     `json:"node,omitempty"`
	Status         string     `json:"status"`
	BytesProcessed int64      `json:"bytesProcessed"`
	BytesUploaded  int64      `json:"bytesUploaded"`
	SizeBytes      int64      `json:"sizeBytes"`
	ProgressPct    float64    `json:"progressPct"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// progress derives the source's completion percentage. It is computed rather
// than stored: the inputs are already in the row, and a finished source is 100%
// whatever its size estimate turned out to be.
func (s RunSource) progress() float64 {
	switch s.Status {
	case SourceSuccess:
		return 100
	case SourcePending:
		return 0
	}
	if s.SizeBytes <= 0 {
		return 0
	}
	pct := float64(s.BytesProcessed) / float64(s.SizeBytes) * 100
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

// RunLogLine is one line of a run's persisted activity log.
type RunLogLine struct {
	TS   time.Time `json:"ts"`
	Line string    `json:"line"`
}

// Disk is one disk (VM) or stream (agent) inside a restore point.
type Disk struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Backup kinds.
const (
	BackupFull        = "full"
	BackupIncremental = "incremental"
)

// Source kinds.
const (
	SourceVM    = "vm"
	SourceAgent = "agent"
)

// Verification results recorded on a restore point. A verification re-hashes
// every stored chunk: it proves the stored data is intact, which is not the
// same as proving a restore of it would boot. Nothing in ProxBack may claim
// the latter until restore testing exists.
const (
	VerifyPassed = "passed"
	VerifyFailed = "failed"
)

// Backup is a restore point.
type Backup struct {
	ID         string `json:"id"`
	JobID      string `json:"jobId"`
	RunID      string `json:"-"`
	SourceKind string `json:"sourceKind"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	// HostID and HostName identify the Proxmox host (cluster) the guest lived
	// in. Two clusters can hold identically named VMs, so a restore point is
	// only unambiguous with them. Empty for agent restore points.
	HostID        string    `json:"hostId,omitempty"`
	HostName      string    `json:"hostName,omitempty"`
	TargetID      string    `json:"targetId"`
	CreatedAt     time.Time `json:"createdAt"`
	SizeBytes     int64     `json:"sizeBytes"`
	UploadedBytes int64     `json:"uploadedBytes"`
	Kind          string    `json:"kind"`
	ParentID      string    `json:"parentId,omitempty"`
	Disks         []Disk    `json:"disks"`
	// LastVerifiedAt, LastVerifyResult and VerifiedBytes are the evidence left
	// by the most recent verification of this point. They describe integrity
	// only — the chunks were re-read and re-hashed — never recoverability.
	LastVerifiedAt   *time.Time `json:"lastVerifiedAt,omitempty"`
	LastVerifyResult string     `json:"lastVerifyResult,omitempty"`
	VerifiedBytes    int64      `json:"verifiedBytes"`
}

// Notification policies for the run webhook.
const (
	NotifyOff      = "off"
	NotifyFailures = "failures"
	NotifyAll      = "all"
)

// ValidNotifyOn reports whether v is one of the accepted notifyOn values.
func ValidNotifyOn(v string) bool {
	switch v {
	case NotifyOff, NotifyFailures, NotifyAll:
		return true
	default:
		return false
	}
}

// Chunk compression modes for the backup engine.
const (
	CompressionZstd = "zstd"
	CompressionOff  = "off"
)

// ValidCompression reports whether v names a supported compression mode.
func ValidCompression(v string) bool {
	switch v {
	case CompressionZstd, CompressionOff:
		return true
	default:
		return false
	}
}

// Bounds of the throughput settings, enforced on PUT /api/settings and again
// when they are read back out of the database.
const (
	MinUploadConcurrency = 1
	MaxUploadConcurrency = 16
	MinUploadLimitMbps   = 0
	MaxUploadLimitMbps   = 10000
)

// Settings holds global server settings.
type Settings struct {
	ServerName  string `json:"serverName"`
	Concurrency int    `json:"concurrency"`
	// WebhookURL receives run notifications; empty disables them.
	WebhookURL string `json:"webhookUrl"`
	// NotifyOn is "off", "failures" or "all".
	NotifyOn string `json:"notifyOn"`
	// UploadConcurrency is how many chunk uploads a backup keeps in flight (1–16).
	UploadConcurrency int `json:"uploadConcurrency"`
	// Compression is the per-chunk compression mode, "zstd" or "off".
	Compression string `json:"compression"`
	// UploadLimitMbps caps upload throughput across the whole server in megabits
	// per second; 0 is unlimited.
	UploadLimitMbps int `json:"uploadLimitMbps"`
}
