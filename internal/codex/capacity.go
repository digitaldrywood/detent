package codex

import (
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

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
	if details, ok := backendcapacity.ClassifyTransientOverload(text, codexOverloadRules); ok {
		return details, true
	}
	return backendcapacity.Classify(text, codexCapacityResetAt(limits), now, codexCapacityRules)
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
