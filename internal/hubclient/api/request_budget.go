package api

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
		c.DeferRequestBudget(path, apiErr.retryAfter)
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
		delay := c.requestBudgetWindow(path).Sub(c.budgetNow())
		c.requestBudgetMu.Unlock()
		if delay <= 0 {
			return nil
		}
		fired, stop := c.budgetTimer(delay)
		select {
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case <-fired:
			stop()
		}
	}
}

// DeferRequestBudget records the refused window. The Hub's budget window is
// the current minute, so a refusal without a usable Retry-After waits for the
// next one.
func (c *Client) DeferRequestBudget(path string, until time.Time) {
	now := c.budgetNow()
	if !until.After(now) {
		until = now.Truncate(time.Minute).Add(time.Minute)
	}
	c.requestBudgetMu.Lock()
	window := c.requestBudgetWindow(path)
	*window = maxTime(*window, until)
	c.requestBudgetMu.Unlock()
}

func (c *Client) requestBudgetWindow(path string) *time.Time {
	path, _, _ = strings.Cut(path, "?")
	if strings.HasSuffix(path, "/go-test-receipts") || strings.HasSuffix(path, "/go-test-receipts/attestations") {
		return &c.receiptBudgetUntil
	}
	return &c.requestBudgetUntil
}

func (c *Client) parseRetryAfter(header string) time.Time {
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		return c.budgetNow().Add(time.Duration(seconds) * time.Second)
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

func (c *Client) budgetTimer(delay time.Duration) (<-chan time.Time, func()) {
	if c.requestBudgetTimer != nil {
		return c.requestBudgetTimer(delay)
	}
	timer := time.NewTimer(delay)
	return timer.C, func() { timer.Stop() }
}

func (c *Client) budgetNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
