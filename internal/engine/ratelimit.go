package engine

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/time/rate"
)

// MaxUploadLimitMbps is the largest accepted upload rate limit.
const MaxUploadLimitMbps = 10000

// The upload rate limit is one process-wide token bucket. Every engine, every
// run and every upload worker draws from it, so an operator who caps the rate
// caps what the whole server does to the uplink — not what one stream does.
var (
	limitMu   sync.RWMutex
	limiter   *rate.Limiter
	limitMbps float64
)

// bitsPerMbps converts the operator-facing megabits per second into bits.
const bitsPerMbps = 1_000_000.0

// SetUploadLimitMbps installs the process-wide upload rate limit, in megabits
// per second. Zero or less removes the limit. Re-installing the value that is
// already in force is a no-op, so building an engine per run does not keep
// refilling the bucket.
func SetUploadLimitMbps(mbps float64) {
	limitMu.Lock()
	defer limitMu.Unlock()
	if mbps < 0 {
		mbps = 0
	}
	if mbps == limitMbps {
		return
	}
	limitMbps = mbps
	if mbps == 0 {
		limiter = nil
		return
	}
	bytesPerSec := mbps * bitsPerMbps / 8
	// The burst is exactly one chunk: enough that a single WaitN can always be
	// satisfied (anything smaller would deadlock the pipeline on its first
	// upload), and small enough that the configured rate is what the link
	// actually sees rather than a rate plus a second of free traffic.
	limiter = rate.NewLimiter(rate.Limit(bytesPerSec), ChunkSize)
}

// UploadLimitMbps reports the process-wide upload limit in megabits per second
// (0 when uploads are unlimited).
func UploadLimitMbps() float64 {
	limitMu.RLock()
	defer limitMu.RUnlock()
	return limitMbps
}

// waitUpload blocks until n bytes may be sent under the process-wide limit.
// Requests larger than the bucket's burst are split rather than rejected, so no
// payload size can wedge the pipeline.
func waitUpload(ctx context.Context, n int) error {
	limitMu.RLock()
	l := limiter
	limitMu.RUnlock()
	if l == nil || n <= 0 {
		return nil
	}
	burst := l.Burst()
	for n > 0 {
		take := n
		if take > burst {
			take = burst
		}
		if err := l.WaitN(ctx, take); err != nil {
			return fmt.Errorf("engine: upload rate limit: %w", err)
		}
		n -= take
	}
	return nil
}
