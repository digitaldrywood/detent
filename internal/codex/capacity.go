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
	HTTP429:      true,
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
	if errors.Is(err, context.Canceled) {
		return backendcapacity.Details{}, false
	}
	text := codexCapacityErrorText(err)
	startup, hasStartupEvidence := startupEvidence(err)
	if hasStartupEvidence || codexStartupOperation(text) {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return backendcapacity.Details{
				Type:    backendcapacity.ErrorTypeTransientOverload,
				Kind:    backendcapacity.StartupTimeoutKind,
				Reason:  "backend startup handshake timed out",
				Trigger: boundedCapacityTrigger(text),
				Startup: startupEvidencePointer(startup, hasStartupEvidence),
			}, true
		case errors.Is(err, io.EOF), strings.Contains(strings.ToLower(text), "process exited"), strings.Contains(strings.ToLower(text), "start codex app-server transport"):
			return backendcapacity.Details{
				Type:    backendcapacity.ErrorTypeTransientOverload,
				Kind:    backendcapacity.StartupFailureKind,
				Reason:  "backend startup handshake failed",
				Trigger: boundedCapacityTrigger(text),
				Startup: startupEvidencePointer(startup, hasStartupEvidence),
			}, true
		}
	}
	evidence, ok := codexProviderCapacityEvidence(err)
	if !ok {
		return backendcapacity.Details{}, false
	}
	if details, ok := backendcapacity.ClassifyTransientOverload(evidence, codexOverloadRules); ok {
		details.Trigger = boundedCapacityTrigger(evidence)
		return details, true
	}
	details, ok := backendcapacity.Classify(evidence, codexCapacityResetAt(limits), now, codexCapacityRules)
	if !ok {
		return backendcapacity.Details{}, false
	}
	if codexSubscriptionLimitText(evidence) {
		details.Reason = "subscription window exhausted"
	}
	details.Trigger = boundedCapacityTrigger(evidence)
	return details, true
}

func startupEvidencePointer(evidence backendcapacity.StartupEvidence, ok bool) *backendcapacity.StartupEvidence {
	if !ok {
		return nil
	}
	return &evidence
}

func codexSubscriptionLimitText(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "chatgpt.com/codex/settings/usage") ||
		strings.Contains(text, "purchase more credits or try again at")
}

func codexProviderCapacityEvidence(err error) (string, bool) {
	parts := make([]string, 0, 3)
	var bodyCarrier interface {
		BackendErrorBody() string
		BackendErrorMessage() string
	}
	if errors.As(err, &bodyCarrier) {
		parts = append(parts, bodyCarrier.BackendErrorBody(), bodyCarrier.BackendErrorMessage())
	}
	var statusCarrier interface {
		BackendErrorStatus() string
	}
	if errors.As(err, &statusCarrier) {
		parts = append(parts, statusCarrier.BackendErrorStatus())
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), len(parts) > 0
}

func boundedCapacityTrigger(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		return value[len(value)-2048:]
	}
	return value
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
		return codexExhaustedCapacityStatus(limits, "live provider status still reports an exhausted subscription window"), true
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
		if bucket.Status == telemetry.RateLimitStatusExhausted {
			return codexExhaustedCapacityStatus(limits, fmt.Sprintf("live provider status reports an exhausted subscription window with %d%% capacity remaining", remaining)), true
		}
		if remaining < codexCapacityAvailableThresholdPercent {
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

func codexExhaustedCapacityStatus(limits *telemetry.RateLimits, detail string) runner.CapacityStatus {
	return runner.CapacityStatus{
		Exhausted: true,
		Detail:    detail,
		Details: backendcapacity.Details{
			Type:    backendcapacity.ErrorTypeUsageLimit,
			Kind:    "subscription_window_exhausted",
			Reason:  "subscription window exhausted",
			ResetAt: codexCapacityResetAt(limits),
		},
	}
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
