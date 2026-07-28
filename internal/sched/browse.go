package sched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"proxback/internal/browse"
	"proxback/internal/engine"
	"proxback/internal/store"
)

/*
File-level browsing of a restore point.

A backup is stored as an ordered list of fixed-size chunks, so a byte offset in
the original stream lands in a predictable chunk. That is what makes it possible
to list what is inside a 250 GiB backup, and pull one file out of it, without
restoring the whole thing first.

What can be browsed depends on what the stream is. An agent file backup is a tar
and is browsable directly. A VM backup is a vzdump VMA stream holding raw disk
images, which has to be taken apart further before any filenames exist; that is
handled in vma.go, and where it cannot read a guest's filesystem it says so
rather than presenting an empty folder as though the backup were empty.
*/

// ErrNoSuchEntry is returned for a path that is not in the restore point.
var ErrNoSuchEntry = errors.New("sched: no such file in this restore point")

// indexTTL is how long a built index is kept in memory.
//
// Building one costs reads against the target, so a browse session that walks
// several folders and then downloads a file should pay for it once. It is a
// cache and never authoritative: dropping it only costs time.
const indexTTL = 30 * time.Minute

type cachedIndex struct {
	index *browse.Index
	built time.Time
}

type browseCache struct {
	mu sync.Mutex
	by map[string]cachedIndex
}

func (c *browseCache) get(backupID string) (*browse.Index, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.by[backupID]
	if !ok || time.Since(e.built) > indexTTL {
		return nil, false
	}
	return e.index, true
}

func (c *browseCache) put(backupID string, idx *browse.Index) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[string]cachedIndex{}
	}
	// Bound the cache: browsing several large restore points in a session
	// should not accumulate every index for half an hour.
	if len(c.by) >= 8 {
		oldest, at := "", time.Now()
		for id, e := range c.by {
			if e.built.Before(at) {
				oldest, at = id, e.built
			}
		}
		delete(c.by, oldest)
	}
	c.by[backupID] = cachedIndex{index: idx, built: time.Now()}
}

// Drop removes a restore point's cached index, so a deleted point does not keep
// answering listings from memory.
func (m *Manager) dropBrowseIndex(backupID string) {
	m.browseIdx.mu.Lock()
	defer m.browseIdx.mu.Unlock()
	delete(m.browseIdx.by, backupID)
}

// readers opens every stream of a restore point for random access.
type openBackup struct {
	manifest *engine.Manifest
	streams  map[string]*browse.StreamReaderAt
}

func (m *Manager) openForBrowse(ctx context.Context, backup *store.Backup) (*openBackup, error) {
	eng, _, err := m.engineFor(ctx, backup.TargetID)
	if err != nil {
		return nil, err
	}
	man, err := eng.ReadManifest(ctx, backup.SourceKind, backup.SourceID, backup.ID)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	ob := &openBackup{manifest: man, streams: map[string]*browse.StreamReaderAt{}}
	for _, dm := range man.Disks {
		ob.streams[dm.Name] = browse.NewStreamReaderAt(ctx, eng, dm)
	}
	return ob, nil
}

// BrowseIndex returns the file index of a restore point, building it if it is
// not already cached.
func (m *Manager) BrowseIndex(ctx context.Context, backup *store.Backup) (*browse.Index, error) {
	if idx, ok := m.browseIdx.get(backup.ID); ok {
		return idx, nil
	}
	ob, err := m.openForBrowse(ctx, backup)
	if err != nil {
		return nil, err
	}
	idx := &browse.Index{BackupID: backup.ID}
	switch backup.SourceKind {
	case store.SourceAgent:
		for _, dm := range ob.manifest.Disks {
			r := ob.streams[dm.Name]
			entries, truncated, err := browse.IndexTar(ctx, r, r.Size(), dm.Name)
			if err != nil {
				return nil, err
			}
			idx.Entries = append(idx.Entries, entries...)
			idx.Truncated = idx.Truncated || truncated
		}
	case store.SourceVM:
		return nil, browse.ErrVMNotIndexable
	default:
		return nil, browse.ErrNotBrowsable
	}
	m.browseIdx.put(backup.ID, idx)
	return idx, nil
}

// BrowseList returns the immediate contents of one directory of a restore point.
func (m *Manager) BrowseList(ctx context.Context, backup *store.Backup, dir string) ([]browse.Entry, bool, error) {
	idx, err := m.BrowseIndex(ctx, backup)
	if err != nil {
		return nil, false, err
	}
	return browse.Children(idx.Entries, dir), idx.Truncated, nil
}

// BrowseSearch finds files by name anywhere in a restore point.
func (m *Manager) BrowseSearch(ctx context.Context, backup *store.Backup, query string, limit int) ([]browse.Entry, error) {
	idx, err := m.BrowseIndex(ctx, backup)
	if err != nil {
		return nil, err
	}
	return browse.Search(idx.Entries, query, limit), nil
}

// BrowseOpenFile returns a reader over one file inside a restore point, along
// with the entry describing it.
//
// Only the extent the file occupies is read, so recovering a 12 KB config out
// of a 200 GiB backup costs a chunk or two rather than a restore.
func (m *Manager) BrowseOpenFile(ctx context.Context, backup *store.Backup, path string) (browse.Entry, io.Reader, error) {
	idx, err := m.BrowseIndex(ctx, backup)
	if err != nil {
		return browse.Entry{}, nil, err
	}
	e, ok := browse.Find(idx.Entries, path)
	if !ok {
		return browse.Entry{}, nil, ErrNoSuchEntry
	}
	if e.Dir {
		return browse.Entry{}, nil, fmt.Errorf("%s is a folder, not a file", e.Path)
	}
	ob, err := m.openForBrowse(ctx, backup)
	if err != nil {
		return browse.Entry{}, nil, err
	}
	if e.Size == 0 {
		return e, bytes.NewReader(nil), nil
	}
	r, ok := ob.streams[e.Stream]
	if !ok {
		return browse.Entry{}, nil, fmt.Errorf("sched: restore point has no stream %q", e.Stream)
	}
	return e, r.SectionReader(e.Offset, e.Size), nil
}
