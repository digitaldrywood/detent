package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) HasRegisteredRunner() bool {
	return c.client.runner != nil
}

func (c *NativeClient) RuntimeEvidence(ctx context.Context, item tracker.NativeWorkItemID, attempt string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	var result tracker.NativeRuntimeEvidence
	supported, err := c.HubFeature(ctx, tracker.NativeRuntimeEvidenceCapability)
	if err != nil {
		return result, err
	}
	if !supported {
		return result, ErrUnavailable
	}
	path := c.base() + "/work-items/" + url.PathEscape(string(item)) + "/runtime"
	params := url.Values{}
	if attempt != "" {
		key := "native_attempt_id"
		if !strings.HasPrefix(attempt, "attempt_") {
			key = "attempt_id"
		}
		params.Set(key, attempt)
	}

	if c.client.runner != nil && len(admission) > 0 {
		supported, err := c.HubFeature(ctx, tracker.NativeAdmissionEvidenceCapability)
		if err != nil {
			return result, err
		}
		if supported {
			raw, err := json.Marshal(admission[0])
			if err != nil {
				return result, err
			}
			params.Set("admission", string(raw))
		}
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	err = c.client.request(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c *NativeClient) GitHubTimings(ctx context.Context, item tracker.NativeWorkItemID, attempt, runnerID string) (tracker.NativeGitHubTimingEvidence, error) {
	var result tracker.NativeGitHubTimingEvidence
	params := url.Values{"native_attempt_id": {attempt}, "runner_id": {runnerID}}
	path := c.base() + "/work-items/" + url.PathEscape(string(item)) + "/runtime/github-timings?" + params.Encode()
	err := c.client.request(ctx, http.MethodGet, path, nil, &result)
	return result, err
}
