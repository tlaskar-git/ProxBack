//go:build !windows

package agent

import (
	"context"
	"errors"
	"log/slog"
)

// RunningAsService always reports false off Windows: systemd runs the agent as
// an ordinary foreground process, so there is no handshake to perform and the
// interactive path is already correct.
func RunningAsService() (bool, error) { return false, nil }

// RunService exists so callers need no build tags of their own. There is no
// service control manager to attach to here.
func RunService(_ string, _ *slog.Logger, _ func(ctx context.Context, log *slog.Logger) error) error {
	return errors.New("agent: service mode is only available on Windows; use systemd on this platform")
}
