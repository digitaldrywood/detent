package hubclient

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A read refused for the runner's per-minute budget waits out the window and is
// sent again instead of failing the caller. Mutations are never replayed
// automatically. The retry count bounds a Hub that keeps refusing.
const maxRequestBudgetRetries = 3

const requestBudgetCode = "runner_request_budget"

// withinRequestBudget is the client's single request-budget path: it waits
// for any window an earlier refusal recorded, sends, and on a budget refusal
// records the window for every request on this client before sending a read
// again. Other refusals, including other 429 codes, return unchanged.
func (c *Client) withinRequestBudget(ctx context.Context, method, path string, send func() error) error {
	replayable := method == http.MethodGet || method == http.MethodHead
	for attempt := 0; ; attempt++ {
		if err := c.waitRequestBudget(ctx, method, path); err != nil {
			return err
		}
		err := send()
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr == nil || apiErr.Status != http.StatusTooManyRequests || apiErr.Code != requestBudgetCode {
			return err
		}
		c.deferRequestBudget(apiErr.retryAfter)
		if !replayable || attempt >= maxRequestBudgetRetries {
			return err
		}
	}
}

func (c *Client) waitRequestBudget(ctx context.Context, method, path string) error {
	path, _, _ = strings.Cut(path, "?")
	if method == http.MethodPost && (strings.HasSuffix(path, "/claims") || strings.HasSuffix(path, "/heartbeat") || strings.Contains(path, "/leases/") || strings.Contains(path, "/runners/") && strings.HasSuffix(path, "/renew")) {
		return nil
	}
	for {
		c.requestBudgetMu.Lock()
		delay := time.Until(c.requestBudgetUntil)
		c.requestBudgetMu.Unlock()
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// deferRequestBudget records the refused window. The Hub's budget window is
// the current minute, so a refusal without a usable Retry-After waits for the
// next one.
func (c *Client) deferRequestBudget(until time.Time) {
	now := time.Now()
	if !until.After(now) {
		until = now.Truncate(time.Minute).Add(time.Minute)
	}
	c.requestBudgetMu.Lock()
	c.requestBudgetUntil = maxTime(c.requestBudgetUntil, until)
	c.requestBudgetMu.Unlock()
}

func parseRetryAfter(header string) time.Time {
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	if parsed, err := http.ParseTime(header); err == nil {
		return parsed
	}
	return time.Time{}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
