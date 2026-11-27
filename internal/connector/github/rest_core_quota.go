package github

import (
	"context"
	"net/http"
	"time"
)

const restCoreQuotaCapacity = 16384
const restCoreQuotaEvictionScan = 16

type restCoreReserveKey struct{}

func WithRESTCoreReserve(ctx context.Context, reserve int64) context.Context {
	return context.WithValue(ctx, restCoreReserveKey{}, RESTResourceReserve("core", reserve))
}

type restCoreQuota struct {
	remaining  int64
	resetAt    time.Time
	probeAfter time.Time
}

func (c *Client) chargeRESTCore(ctx context.Context, key string, now time.Time) error {
	var reserve int64
	if requested, ok := ctx.Value(restCoreReserveKey{}).(int64); ok {
		reserve = requested
	}
	probe, err := c.restBackoffs.chargeCoreQuota(key, now, reserve, true)
	if err != nil || !probe {
		return err
	}
	result, err := c.restProbe(ctx, http.MethodGet, "/rate_limit", nil)
	if err != nil || result.backoffKey != key || result.StatusCode != http.StatusOK {
		return restCoreBudgetReserved(time.Minute)
	}
	_, err = c.restBackoffs.chargeCoreQuota(key, time.Now(), reserve, false)
	return err
}

func (r *restBackoffRegistry) chargeCoreQuota(key string, now time.Time, reserve int64, allowProbe bool) (bool, error) {
	if r == nil || key == "" {
		if reserve > 0 {
			return false, restCoreBudgetReserved(time.Minute)
		}
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	quota, tracked := r.core[key]
	if quota.resetAt.After(now) {
		if reserve > 0 && quota.remaining <= reserve {
			return false, restCoreBudgetReserved(quota.resetAt.Sub(now))
		}
		quota.remaining = max(0, quota.remaining-1)
		r.core[key] = quota
		return false, nil
	}
	if reserve == 0 {
		return false, nil
	}
	if !allowProbe || quota.probeAfter.After(now) {
		return false, restCoreBudgetReserved(quota.probeAfter.Sub(now))
	}
	if !tracked && len(r.core) >= restCoreQuotaCapacity {
		scanned := 0
		for credential, entry := range r.core {
			if !entry.resetAt.After(now) && !entry.probeAfter.After(now) {
				delete(r.core, credential)
				break
			}
			scanned++
			if scanned == restCoreQuotaEvictionScan {
				return false, restCoreBudgetReserved(time.Minute)
			}
		}
	}
	quota.probeAfter = now.Add(time.Minute)
	r.core[key] = quota
	return true, nil
}

func (r *restBackoffRegistry) observeCoreQuota(key string, remaining int64, resetAt time.Time) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	quota, active := r.core[key]
	if !active || resetAt.Before(quota.resetAt) {
		return
	}
	if resetAt.Equal(quota.resetAt) {
		remaining = min(remaining, quota.remaining)
	}
	r.core[key] = restCoreQuota{remaining: remaining, resetAt: resetAt}
}

func restCoreBudgetReserved(retryAfter time.Duration) error {
	return &StatusError{StatusCode: http.StatusTooManyRequests, Err: ErrRESTBudgetReserved, RetryAfter: max(time.Second, retryAfter)}
}
