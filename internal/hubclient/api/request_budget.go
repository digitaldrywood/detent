package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const requestBudgetCode = "runner_request_budget"

func (c *Client) withinRequestBudget(ctx context.Context, method, path string, replayMutation bool, send func() error) error {
	replayable := replayMutation || method == http.MethodGet || method == http.MethodHead
	for {
		if err := c.waitRequestBudget(ctx, method, path); err != nil {
			return err
		}
		err := send()
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr == nil || apiErr.Status != http.StatusTooManyRequests || apiErr.Code != requestBudgetCode {
			return err
		}
		c.DeferRequestBudget(path, apiErr.retryAfter)
		if !replayable {
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
		delay := time.Until(*c.requestBudgetWindow(path))
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

// DeferRequestBudget records the refused window. The Hub's budget window is
// the current minute, so a refusal without a usable Retry-After waits for the
// next one.
func (c *Client) DeferRequestBudget(path string, until time.Time) {
	now := time.Now()
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
