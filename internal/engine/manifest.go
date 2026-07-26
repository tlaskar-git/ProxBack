package engine

import (
	"fmt"
	"time"
)

// ChunkSize is the fixed chunk size used for every backup stream.
const ChunkSize = 4 << 20 // 4 MiB

// Chunk is one content-addressed chunk reference inside a manifest.
type Chunk struct {
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// DiskManifest is the ordered chunk list of one disk (VM) or stream (agent).
type DiskManifest struct {
	Name      string  `json:"name"`
	SizeBytes int64   `json:"sizeBytes"`
	Chunks    []Chunk `json:"chunks"`
}

// Manifest is the backup point descriptor stored in S3.
type Manifest struct {
	BackupID      string         `json:"backupId"`
	JobID         string         `json:"jobId"`
	JobName       string         `json:"jobName"`
	RunID         string         `json:"runId"`
	SourceKind    string         `json:"sourceKind"` // vm | agent
	SourceID      string         `json:"sourceId"`
	SourceName    string         `json:"sourceName"`
	TargetID      string         `json:"targetId"`
	CreatedAt     time.Time      `json:"createdAt"`
	Kind          string         `json:"kind"` // full | incremental
	ParentID      string         `json:"parentId,omitempty"`
	SizeBytes     int64          `json:"sizeBytes"`
	UploadedBytes int64          `json:"uploadedBytes"`
	ChunkSize     int64          `json:"chunkSize"`
	Disks         []DiskManifest `json:"disks"`
}

// Backup kinds.
const (
	KindFull        = "full"
	KindIncremental = "incremental"
)

// Kind returns the backup kind implied by the presence of a parent restore point.
// A backup is incremental if and only if a previous backup exists for the same
// source on the same target.
func Kind(parentID string) string {
	if parentID == "" {
		return KindFull
	}
	return KindIncremental
}

// ChunkKey is the S3 key of a deduplicated chunk.
func ChunkKey(sha256hex string) string { return "chunks/" + sha256hex }

// ChunkPrefix is the S3 prefix holding all chunks of a target.
const ChunkPrefix = "chunks/"

// ManifestPrefix is the S3 prefix holding all manifests of a target.
const ManifestPrefix = "manifests/"

// ManifestKey is the S3 key of a backup manifest.
func ManifestKey(sourceKind, sourceID, backupID string) string {
	return fmt.Sprintf("manifests/%s/%s/%s.json", sourceKind, sourceID, backupID)
}

// SourceManifestPrefix is the S3 prefix holding all manifests of one source.
func SourceManifestPrefix(sourceKind, sourceID string) string {
	return fmt.Sprintf("manifests/%s/%s/", sourceKind, sourceID)
}

// TotalSize sums the disk sizes of a manifest.
func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, d := range m.Disks {
		n += d.SizeBytes
	}
	return n
}

// ChunkCount counts all chunk references in a manifest.
func (m *Manifest) ChunkCount() int {
	n := 0
	for _, d := range m.Disks {
		n += len(d.Chunks)
	}
	return n
}
