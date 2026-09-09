package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type restRecoveryEvidence struct {
	path     string
	resource string
}

func (c *Client) probeRESTRecovery(ctx context.Context, minimumRemaining int64, path string) (connector.RESTRateLimit, error) {
	result, err := c.restProbe(ctx, http.MethodGet, "/rate_limit", nil)
	if err != nil {
		return connector.RESTRateLimit{}, err
	}
	quota, err := restRecoveryQuota(result, "core", time.Now())
	if err != nil {
		return connector.RESTRateLimit{}, err
	}
	key := result.backoffKey
	if err := c.restBackoffError(key, time.Now()); err != nil {
		return connector.RESTRateLimit{}, err
	}
	if quota.Remaining <= minimumRemaining {
		return quota, nil
	}

	evidence := c.restBackoffs.failure(key)
	resource := "core"
	if path == "/user" && strings.HasPrefix(result.credentialIdentity, "github-app-installation:") {
		path = "/installation/repositories?per_page=1"
	}
	fallbackPath := path
	if evidence != nil {
		resource = evidence.resource
		if evidence.path != "" {
			path = evidence.path
		}
	}
	result, err = c.restProbe(ctx, http.MethodGet, path, nil)
	if err != nil {
		return connector.RESTRateLimit{}, err
	}
	if path != fallbackPath && (result.StatusCode == http.StatusNotFound || result.StatusCode == http.StatusUnprocessableEntity) {
		if err := c.restBackoffError(key, time.Now()); err != nil {
			return connector.RESTRateLimit{}, err
		}
		result, err = c.restProbe(ctx, http.MethodGet, fallbackPath, nil)
		if err != nil {
			return connector.RESTRateLimit{}, err
		}
	}
	quota, err = restRecoveryQuota(result, resource, time.Now())
	if err != nil {
		return connector.RESTRateLimit{}, err
	}
	if result.backoffKey != key {
		return connector.RESTRateLimit{}, fmt.Errorf("%w: credential changed during REST recovery", ErrInvalidResponse)
	}
	if quota.Remaining <= minimumRemaining {
		return quota, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.restBackoffKey != key || c.restBackoffUntil.After(time.Now()) || !c.restBackoffs.recover(key, evidence, time.Now()) {
		return connector.RESTRateLimit{}, fmt.Errorf("%w: newer REST exhaustion evidence", ErrRateLimited)
	}
	c.restBackoffUntil = time.Time{}
	c.restRateLimitStatus = false
	c.restReserveHeld = false
	c.restRateLimit = quota
	if c.restRateLimits == nil {
		c.restRateLimits = make(map[string]connector.RESTRateLimit)
	}
	c.restRateLimits[quota.Resource] = quota
	c.hasRestRateLimit = true
	return quota, nil
}

func restRecoveryQuota(result restProbeResult, resource string, now time.Time) (connector.RESTRateLimit, error) {
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return connector.RESTRateLimit{}, classifyStatusAt(result.StatusCode, result.Headers, []byte(result.FullBody), now)
	}
	limit, hasLimit := int64Header(result.Headers, "X-RateLimit-Limit")
	remaining, hasRemaining := int64Header(result.Headers, "X-RateLimit-Remaining")
	reset, hasReset := int64Header(result.Headers, "X-RateLimit-Reset")
	used, _ := int64Header(result.Headers, "X-RateLimit-Used")
	actualResource := strings.TrimSpace(result.Headers.Get("X-RateLimit-Resource"))
	if !hasLimit || limit <= 0 || !hasRemaining || remaining < 0 || remaining > limit || !hasReset || !time.Unix(reset, 0).After(now) || actualResource != resource {
		return connector.RESTRateLimit{}, ErrInvalidResponse
	}
	return connector.RESTRateLimit{
		Limit: limit, Remaining: remaining, Used: used, Resource: resource,
		ResetAt: time.Unix(reset, 0).UTC(), UpdatedAt: now,
	}, nil
}

func (r *restBackoffRegistry) failure(key string) *restRecoveryEvidence {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.failures[key]
}

func (r *restBackoffRegistry) recover(key string, evidence *restRecoveryEvidence, now time.Time) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures[key] != evidence || r.untils[key].After(now) {
		return false
	}
	delete(r.untils, key)
	delete(r.failures, key)
	return true
}
