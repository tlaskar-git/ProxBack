// Package notify delivers ProxBack run notifications to an operator supplied
// webhook URL. The payload is plain JSON, usable by ntfy, Gotify, a Discord
// webhook proxy or any automation endpoint.
//
// Delivery is strictly best effort: a notification must never change the
// outcome of a backup, restore or verify run, so callers either ignore the
// error or use Notify, which logs it.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Timeout bounds one delivery attempt.
const Timeout = 10 * time.Second

// EventRunFinished is the only event ProxBack emits today.
const EventRunFinished = "run.finished"

// Payload is the JSON body POSTed to the webhook URL.
type Payload struct {
	Event          string    `json:"event"`
	Server         string    `json:"server"`
	Job            string    `json:"job"`
	Kind           string    `json:"kind"` // vm | agent | restore | verify
	Status         string    `json:"status"`
	BytesProcessed int64     `json:"bytesProcessed"`
	BytesUploaded  int64     `json:"bytesUploaded"`
	DedupRatio     float64   `json:"dedupRatio"`
	Error          string    `json:"error,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt"`
	DurationSec    float64   `json:"durationSec"`
}

// Notifier posts payloads to webhook URLs.
type Notifier struct {
	hc  *http.Client
	log *slog.Logger
}

// New builds a notifier with the 10 s delivery timeout.
func New(log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{hc: &http.Client{Timeout: Timeout}, log: log}
}

// Send POSTs one payload and reports the outcome. The error is only meant for
// the operator-facing test endpoint; run paths must not act on it.
func (n *Notifier) Send(ctx context.Context, url string, p Payload) error {
	if url == "" {
		return fmt.Errorf("notify: no webhook url configured")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("notify: encode payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ProxBack")
	resp, err := n.hc.Do(req)
	if err != nil {
		return fmt.Errorf("notify: post to webhook: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: webhook returned http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	n.log.Debug("webhook delivered", "status", resp.StatusCode, "event", p.Event, "job", p.Job)
	return nil
}

// Notify sends a payload and logs any failure, swallowing the error.
func (n *Notifier) Notify(ctx context.Context, url string, p Payload) {
	if err := n.Send(ctx, url, p); err != nil {
		n.log.Warn("could not deliver run notification", "job", p.Job, "status", p.Status, "error", err)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
