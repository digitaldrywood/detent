package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const codexCapacityAvailableThresholdPercent = 5

var codexCapacityRules = backendcapacity.Rules{
	Kinds: []string{
		"usageLimitExceeded",
		"usage_limit_exceeded",
		"quota_exceeded",
		"insufficient_quota",
		"rate_limit_exceeded",
		"rateLimitExceeded",
		"RESOURCE_EXHAUSTED",
		"dailyLimitExceeded",
	},
	Phrases: []string{
		"you've hit your usage limit",
		"usage limit reached",
		"quota exceeded",
		"insufficient quota",
	},
	RequireReset: true,
}

var codexOverloadRules = backendcapacity.Rules{
	Kinds: []string{
		"serverOverloaded",
		"overloaded_error",
		"model_at_capacity",
	},
	Phrases: []string{
		"selected model is at capacity",
		"model is at capacity",
		"model at capacity",
		"server overloaded",
		"temporarily overloaded",
	},
	HTTP5xx: true,
}

func (b *AgentBackend) ClassifyCapacityError(err error, limits *telemetry.RateLimits, now time.Time) (backendcapacity.Details, bool) {
	if b == nil || err == nil {
		return backendcapacity.Details{}, false
	}
	return ClassifyCapacityError(err, limits, now)
}

func ClassifyCapacityError(err error, limits *telemetry.RateLimits, now time.Time) (backendcapacity.Details, bool) {
	if err == nil {
		return backendcapacity.Details{}, false
	}
	text := codexCapacityErrorText(err)
	if codexStartupOperation(text) {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return backendcapacity.Details{
				Type:   backendcapacity.ErrorTypeTransientOverload,
				Kind:   backendcapacity.StartupTimeoutKind,
				Reason: "backend startup handshake timed out",
			}, true
		case errors.Is(err, io.EOF), strings.Contains(strings.ToLower(text), "process exited"), strings.Contains(strings.ToLower(text), "start codex app-server transport"):
			return backendcapacity.Details{
				Type:   backendcapacity.ErrorTypeTransientOverload,
				Kind:   backendcapacity.StartupFailureKind,
				Reason: "backend startup handshake failed",
			}, true
		}
	}
	if details, ok := backendcapacity.ClassifyTransientOverload(text, codexOverloadRules); ok {
		return details, true
	}
	return backendcapacity.Classify(text, codexCapacityResetAt(limits), now, codexCapacityRules)
}

func codexStartupOperation(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if strings.Contains(text, "start codex app-server transport") {
		return true
	}
	for _, operation := range []string{"initialize", "thread/start", "thread/resume", "turn/start"} {
		if strings.Contains(text, "wait for "+operation+" response") {
			return true
		}
	}
	return false
}

func (b *AgentBackend) CapacityStatus(limits *telemetry.RateLimits) (runner.CapacityStatus, bool) {
	if b == nil {
		return runner.CapacityStatus{}, false
	}
	return CapacityStatus(limits)
}

func CapacityStatus(limits *telemetry.RateLimits) (runner.CapacityStatus, bool) {
	if limits == nil {
		return runner.CapacityStatus{}, false
	}
	if strings.TrimSpace(limits.ReachedType) != "" {
		return runner.CapacityStatus{Detail: "live provider status still reports an exhausted window"}, true
	}
	minimumRemaining := int64(101)
	found := false
	for _, bucket := range []*telemetry.RateLimitBucket{limits.Primary, limits.Secondary} {
		if bucket == nil || bucket.Limit <= 0 {
			continue
		}
		found = true
		remaining := bucket.Remaining * 100 / bucket.Limit
		minimumRemaining = min(minimumRemaining, remaining)
		if bucket.Status == telemetry.RateLimitStatusExhausted || remaining < codexCapacityAvailableThresholdPercent {
			return runner.CapacityStatus{Detail: fmt.Sprintf("live provider status reports %d%% capacity remaining", remaining)}, true
		}
	}
	if !found {
		return runner.CapacityStatus{}, false
	}
	return runner.CapacityStatus{
		Available: true,
		Detail:    fmt.Sprintf("live provider status reports %d%% capacity remaining", minimumRemaining),
	}, true
}

func codexCapacityErrorText(err error) string {
	text := strings.TrimSpace(err.Error())
	var carrier interface {
		BackendErrorBody() string
		BackendErrorMessage() string
	}
	if errors.As(err, &carrier) {
		for _, value := range []string{carrier.BackendErrorBody(), carrier.BackendErrorMessage()} {
			value = strings.TrimSpace(value)
			if value == "" || strings.Contains(text, value) {
				continue
			}
			if text != "" {
				text += "\n"
			}
			text += value
		}
	}
	return text
}

func codexCapacityResetAt(limits *telemetry.RateLimits) *time.Time {
	if limits == nil {
		return nil
	}
	buckets := []*telemetry.RateLimitBucket{limits.Primary, limits.Secondary}
	if strings.Contains(strings.ToLower(limits.ReachedType), "secondary") {
		buckets[0], buckets[1] = buckets[1], buckets[0]
	}
	for _, bucket := range buckets {
		if bucket == nil || bucket.ResetAt == nil || bucket.Status != telemetry.RateLimitStatusExhausted {
			continue
		}
		resetAt := bucket.ResetAt.UTC()
		return &resetAt
	}
	for _, bucket := range buckets {
		if bucket == nil || bucket.ResetAt == nil {
			continue
		}
		resetAt := bucket.ResetAt.UTC()
		return &resetAt
	}
	return nil
}
