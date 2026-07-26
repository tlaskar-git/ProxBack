// Package s3sim provides an S3-compatible server for tests and local runs.
//
// It is backed by gofakes3 (in-memory, or bolt-file when a directory is given).
// SigV4 signatures are accepted without validation, which is what the ProxBack
// engine (aws-sdk-go-v2 with a custom endpoint and path-style addressing)
// needs from a simulator.
package s3sim

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3bolt"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// Server is a running simulator instance.
type Server struct {
	Handler http.Handler
	closers []func() error
}

// Close releases any backing resources.
func (s *Server) Close() error {
	var firstErr error
	for _, c := range s.closers {
		if err := c(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// New builds a simulator. When dir is empty the backend is in-memory, otherwise a
// bolt database is created inside dir.
func New(dir string) (*Server, error) {
	var backend gofakes3.Backend
	srv := &Server{}
	if dir == "" {
		backend = s3mem.New()
	} else {
		b, err := s3bolt.NewFile(filepath.Join(dir, "s3sim.bolt"))
		if err != nil {
			return nil, fmt.Errorf("s3sim: open bolt backend: %w", err)
		}
		backend = b
	}
	faker := gofakes3.New(backend,
		gofakes3.WithAutoBucket(true),
		gofakes3.WithLogger(gofakes3.DiscardLog()),
	)
	srv.Handler = faker.Server()
	return srv, nil
}
