package hubclient

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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

func (c *Client) observeRequestBudget(response *http.Response) {
	if response.StatusCode != http.StatusTooManyRequests {
		return
	}
	now := time.Now()
	var until time.Time
	if seconds, err := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if parsed, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
		until = parsed
	}
	if !until.After(now) {
		return
	}
	c.requestBudgetMu.Lock()
	c.requestBudgetUntil = maxTime(c.requestBudgetUntil, until)
	c.requestBudgetMu.Unlock()
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
