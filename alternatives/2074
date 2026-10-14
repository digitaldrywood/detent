package observability

import (
	"strings"
	"time"
)

type Class string

const (
	ClassFault       Class = "fault"
	ClassDiagnostic  Class = "diagnostic"
	ClassReviewQueue Class = "review_queue"
)

func Normalize(class Class, fallback Class) Class {
	switch Class(strings.ToLower(strings.TrimSpace(string(class)))) {
	case ClassFault:
		return ClassFault
	case ClassDiagnostic:
		return ClassDiagnostic
	case ClassReviewQueue:
		return ClassReviewQueue
	default:
		return fallback
	}
}

func Staleness(waitingOnHuman bool) Class {
	if waitingOnHuman {
		return ClassReviewQueue
	}
	return ClassDiagnostic
}

func Dispatch(stalled bool, reason string) Class {
	if !stalled {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "authorization_selector_declined",
		"ownership_assignee_required",
		"budget_hard_hold",
		"lifetime_limit",
		"tracker_unavailable",
		"forge_unavailable",
		"ci_unavailable",
		"project_failure_breaker",
		"hydrate_failed",
		"invalid_candidate":
		return ClassFault
	case "awaiting_gate", "artifact_gate_wait_status":
		return ClassReviewQueue
	default:
		return ClassDiagnostic
	}
}

func BackendOutage(kind string) Class {
	if strings.EqualFold(strings.TrimSpace(kind), "github_rest_rate_limit") {
		return ClassDiagnostic
	}
	return ClassFault
}

func FailureBreaker(eligibleCandidateCount *int, itemCount int, parkedItemCount int) Class {
	if eligibleCandidateCount != nil && *eligibleCandidateCount == 0 && itemCount > 0 && itemCount == parkedItemCount {
		return ClassDiagnostic
	}
	return ClassFault
}

func DispatchRecovery(status string, resumeAt time.Time, observedAt time.Time) Class {
	if strings.EqualFold(strings.TrimSpace(status), "waiting") &&
		!resumeAt.IsZero() &&
		!observedAt.IsZero() &&
		!resumeAt.After(observedAt) {
		return ClassFault
	}
	return ClassDiagnostic
}

func Merge(classes ...Class) Class {
	merged := Class("")
	for _, class := range classes {
		switch Normalize(class, "") {
		case ClassFault:
			return ClassFault
		case ClassDiagnostic:
			merged = ClassDiagnostic
		case ClassReviewQueue:
			if merged == "" {
				merged = ClassReviewQueue
			}
		}
	}
	return merged
}
