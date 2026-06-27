package onboarding

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	DeliveryProfileFullAutopilot      = "full_autopilot"
	DeliveryProfileReviewGate         = "review_gate"
	DeliveryProfileConservativeManual = "conservative_manual"
	DeliveryProfileAutonomousDelivery = DeliveryProfileFullAutopilot
	DeliveryProfileConservativeReview = DeliveryProfileConservativeManual
)

type DeliveryProfileSettings struct {
	ID                           string
	Label                        string
	KanbanMode                   string
	AutoPromoteEnabled           bool
	AutoPromoteQuietSeconds      int
	GateRequireAutomatedReview   bool
	DependencyAutoUnblockEnabled bool
	MergingConcurrency           int
}

type DeliveryProfileSummary struct {
	EffectiveDeliveryProfile      string   `json:"effective_delivery_profile"`
	EffectiveDeliveryProfileLabel string   `json:"effective_delivery_profile_label"`
	KanbanMode                    string   `json:"kanban_mode"`
	KanbanBehavior                string   `json:"kanban_behavior"`
	GateRequiresAutomatedReview   bool     `json:"gate_requires_automated_review"`
	GateBehavior                  string   `json:"gate_behavior"`
	AutoPromoteEnabled            bool     `json:"auto_promote_enabled"`
	AutoPromoteQuietSeconds       int      `json:"auto_promote_quiet_seconds"`
	AutoPromotionBehavior         string   `json:"auto_promotion_behavior"`
	QuietWindowBehavior           string   `json:"quiet_window_behavior"`
	DependencyAutoUnblockEnabled  bool     `json:"dependency_auto_unblock_enabled"`
	DependencyAutoUnblockBehavior string   `json:"dependency_auto_unblock_behavior"`
	MergingConcurrency            int      `json:"merging_concurrency"`
	MergeConcurrencyBehavior      string   `json:"merge_concurrency_behavior"`
	StopBehavior                  string   `json:"stop_behavior"`
	StopConditions                []string `json:"stop_conditions"`
}

func NormalizeDeliveryProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "full", "full-autopilot", "full_autopilot", "autopilot", "maximum", "max", "autonomous", "autonomous-delivery", "autonomous_delivery":
		return DeliveryProfileFullAutopilot
	case "review", "review-gate", "review_gate", "human_review", "human-review":
		return DeliveryProfileReviewGate
	case "conservative", "conservative-manual", "conservative_manual", "manual", "conservative-review", "conservative_review":
		return DeliveryProfileConservativeManual
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func DeliveryProfile(value string) (DeliveryProfileSettings, bool) {
	switch NormalizeDeliveryProfile(value) {
	case DeliveryProfileFullAutopilot:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileFullAutopilot,
			Label:                        "Full autopilot",
			KanbanMode:                   "integration",
			AutoPromoteEnabled:           true,
			AutoPromoteQuietSeconds:      0,
			GateRequireAutomatedReview:   false,
			DependencyAutoUnblockEnabled: true,
			MergingConcurrency:           1,
		}, true
	case DeliveryProfileReviewGate:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileReviewGate,
			Label:                        "Review gate",
			KanbanMode:                   "integration",
			AutoPromoteEnabled:           false,
			AutoPromoteQuietSeconds:      600,
			GateRequireAutomatedReview:   false,
			DependencyAutoUnblockEnabled: false,
			MergingConcurrency:           1,
		}, true
	case DeliveryProfileConservativeManual:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileConservativeManual,
			Label:                        "Conservative/manual",
			KanbanMode:                   "read_only",
			AutoPromoteEnabled:           false,
			AutoPromoteQuietSeconds:      600,
			GateRequireAutomatedReview:   true,
			DependencyAutoUnblockEnabled: false,
			MergingConcurrency:           1,
		}, true
	default:
		return DeliveryProfileSettings{}, false
	}
}

func SummarizeDeliveryProfile(value string) (DeliveryProfileSummary, bool) {
	settings, ok := DeliveryProfile(value)
	if !ok {
		return DeliveryProfileSummary{}, false
	}
	return DeliveryProfileSummary{
		EffectiveDeliveryProfile:      settings.ID,
		EffectiveDeliveryProfileLabel: settings.Label,
		KanbanMode:                    settings.KanbanMode,
		KanbanBehavior:                kanbanBehavior(settings),
		GateRequiresAutomatedReview:   settings.GateRequireAutomatedReview,
		GateBehavior:                  gateBehavior(settings),
		AutoPromoteEnabled:            settings.AutoPromoteEnabled,
		AutoPromoteQuietSeconds:       settings.AutoPromoteQuietSeconds,
		AutoPromotionBehavior:         autoPromotionBehavior(settings),
		QuietWindowBehavior:           quietWindowBehavior(settings),
		DependencyAutoUnblockEnabled:  settings.DependencyAutoUnblockEnabled,
		DependencyAutoUnblockBehavior: dependencyAutoUnblockBehavior(settings),
		MergingConcurrency:            settings.MergingConcurrency,
		MergeConcurrencyBehavior:      mergeConcurrencyBehavior(settings),
		StopBehavior:                  "Existing validation, CI, unresolved review feedback, dependency blockers, mergeability, and gate failures still stop progress.",
		StopConditions: []string{
			"validation failures",
			"CI failures",
			"unresolved review feedback",
			"dependency blockers",
			"mergeability problems",
			"gate failures",
		},
	}, true
}

func DeliveryProfileAnswerExpansion(value string) (map[string]string, bool) {
	settings, ok := DeliveryProfile(value)
	if !ok {
		return nil, false
	}
	return map[string]string{
		"KANBAN_MODE":                           settings.KanbanMode,
		"AUTO_PROMOTE_ENABLED":                  strconv.FormatBool(settings.AutoPromoteEnabled),
		"AUTO_PROMOTE_QUIET_SECONDS":            strconv.Itoa(settings.AutoPromoteQuietSeconds),
		"GATE_REQUIRE_AUTOMATED_REVIEW":         strconv.FormatBool(settings.GateRequireAutomatedReview),
		"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW": strconv.FormatBool(settings.GateRequireAutomatedReview),
		"DEPENDENCY_AUTO_UNBLOCK_ENABLED":       strconv.FormatBool(settings.DependencyAutoUnblockEnabled),
		"MERGING_CONCURRENCY":                   strconv.Itoa(settings.MergingConcurrency),
	}, true
}

func SortedDeliveryProfileAnswerKeys(answers map[string]string) []string {
	keys := make([]string, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func kanbanBehavior(settings DeliveryProfileSettings) string {
	switch settings.KanbanMode {
	case "integration":
		return "Detent can move issues through the configured workflow states instead of only observing them."
	case "read_only":
		return "Detent reads workflow status without mutating Kanban state."
	default:
		return fmt.Sprintf("Detent uses Kanban mode %q.", settings.KanbanMode)
	}
}

func gateBehavior(settings DeliveryProfileSettings) string {
	if settings.GateRequireAutomatedReview {
		return "Automated GitHub PR review is required before the command gate and promotion checks can pass."
	}
	return "No automated GitHub PR review is required when the command gate is passing and the workflow says so."
}

func autoPromotionBehavior(settings DeliveryProfileSettings) string {
	if settings.AutoPromoteEnabled {
		return "Detent automatically promotes eligible work from `Human Review` to `Merging` when the linked PR, local gate, CI, and guardrails pass."
	}
	return "Detent stops in `Human Review` until an operator approves promotion to `Merging`."
}

func quietWindowBehavior(settings DeliveryProfileSettings) string {
	if settings.AutoPromoteEnabled && settings.AutoPromoteQuietSeconds == 0 {
		return "There is no quiet-window delay before promotion."
	}
	if settings.AutoPromoteEnabled {
		return fmt.Sprintf("Detent waits %d seconds of quiet time before promotion after readiness checks pass.", settings.AutoPromoteQuietSeconds)
	}
	return fmt.Sprintf("Auto-promotion is disabled; the %d-second quiet window only matters if auto-promotion is enabled later.", settings.AutoPromoteQuietSeconds)
}

func dependencyAutoUnblockBehavior(settings DeliveryProfileSettings) string {
	if settings.DependencyAutoUnblockEnabled {
		return "Dependency-waiting `Blocked` issues can move back to `Todo` when declared blockers are terminal or merged."
	}
	return "Dependency-waiting `Blocked` issues remain `Blocked` until a human or workflow moves them."
}

func mergeConcurrencyBehavior(settings DeliveryProfileSettings) string {
	if settings.MergingConcurrency == 1 {
		return "`Merging` remains serialized for this project."
	}
	return fmt.Sprintf("Up to %d issues can be in `Merging` concurrently for this project.", settings.MergingConcurrency)
}
