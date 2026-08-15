package claudecode

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var claudeCapacityRules = backendcapacity.Rules{
	Kinds: []string{
		"rate_limit_error",
		"usage_limit_exceeded",
		"quota_exceeded",
		"rateLimitExceeded",
		"RESOURCE_EXHAUSTED",
		"dailyLimitExceeded",
	},
	Phrases: []string{
		"usage limit reached",
		"rate limit reached",
		"you've hit your limit",
		"you've hit your session limit",
		"quota exceeded",
	},
	RequireReset: true,
}

var claudeOverloadRules = backendcapacity.Rules{
	Kinds: []string{
		"overloaded_error",
		"serverOverloaded",
	},
	Phrases: []string{
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
	if details, ok := backendcapacity.ClassifyTransientOverload(err.Error(), claudeOverloadRules); ok {
		return details, true
	}
	return backendcapacity.Classify(err.Error(), claudeCapacityResetAt(limits), now, claudeCapacityRules)
}

func claudeCapacityResetAt(limits *telemetry.RateLimits) *time.Time {
	if limits == nil {
		return nil
	}
	buckets := []*telemetry.RateLimitBucket{limits.Primary, limits.Secondary}
	reachedType := strings.TrimSpace(limits.ReachedType)
	if reachedType != "" && !strings.EqualFold(reachedType, "five_hour") {
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
