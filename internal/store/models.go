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
type EnrollToken struct {
	Token     string     `json:"token"`
	Purpose   string     `json:"-"`
	CreatedAt time.Time  `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"-"`
}

// DefaultHelperPort is the port a node helper listens on unless told otherwise.
const DefaultHelperPort = 8007

// NodeHelper is an enrolled ProxBack node helper: the root daemon on a Proxmox
// node that wraps vzdump/qmrestore for agentless VM image backup. AccessSecret
// is the credential the server presents to the helper and is only populated by
// accessors that decrypt it; it is never serialised.
type NodeHelper struct {
	ID           string     `json:"id"`
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
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"` // vm | agent
	TargetID  string     `json:"targetId"`
	Schedule  string     `json:"schedule"` // "manual" or a 5 field cron spec
	Retention int        `json:"retention"`
	Enabled   bool       `json:"enabled"`
	Sources   JobSources `json:"sources"`
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
	DedupRatio     float64    `json:"dedupRatio"`
	Error          string     `json:"error,omitempty"`
	ProgressPct    float64    `json:"progressPct"`
	CurrentStep    string     `json:"currentStep"`
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

// Backup is a restore point.
type Backup struct {
	ID            string    `json:"id"`
	JobID         string    `json:"jobId"`
	RunID         string    `json:"-"`
	SourceKind    string    `json:"sourceKind"`
	SourceID      string    `json:"sourceId"`
	SourceName    string    `json:"sourceName"`
	TargetID      string    `json:"targetId"`
	CreatedAt     time.Time `json:"createdAt"`
	SizeBytes     int64     `json:"sizeBytes"`
	UploadedBytes int64     `json:"uploadedBytes"`
	Kind          string    `json:"kind"`
	ParentID      string    `json:"parentId,omitempty"`
	Disks         []Disk    `json:"disks"`
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
