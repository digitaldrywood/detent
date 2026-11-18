package api

import (
	"context"
	"crypto/rand"
	"math/big"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	terminalReleaseTimeout    = 10 * time.Second
	terminalReleaseBackoff    = 250 * time.Millisecond
	terminalReleaseBackoffMax = 2 * time.Second
)

func (c *NativeClient) ReleaseTerminal(ctx context.Context, lease tracker.NativeLease, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalReleaseTimeout)
	defer cancel()
	backoff := terminalReleaseBackoff
	for attempt := 0; ; attempt++ {
		err := c.Release(ctx, lease, reason)
		if err == nil || ClaimLost(err) || !NativeTransportUnavailable(err) || ctx.Err() != nil {
			return err
		}
		if attempt == 0 {
			continue
		}
		if !sleepContext(ctx, jitteredBackoff(backoff)) {
			return err
		}
		backoff = min(terminalReleaseBackoffMax, backoff*2)
	}
}

func jitteredBackoff(backoff time.Duration) time.Duration {
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(backoff/2)+1))
	if err != nil {
		return backoff
	}
	return backoff/2 + time.Duration(jitter.Int64())
}
