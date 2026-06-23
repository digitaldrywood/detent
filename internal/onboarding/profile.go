package onboarding

import (
	"sort"
	"strconv"
	"strings"
)

const (
	DeliveryProfileConservativeReview = "conservative_review"
	DeliveryProfileAutonomousDelivery = "autonomous_delivery"
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

func NormalizeDeliveryProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "conservative", "conservative-review", "review", "human_review", "human-review", DeliveryProfileConservativeReview:
		return DeliveryProfileConservativeReview
	case "autonomous", "autonomous-delivery", "delivery", DeliveryProfileAutonomousDelivery:
		return DeliveryProfileAutonomousDelivery
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func DeliveryProfile(value string) (DeliveryProfileSettings, bool) {
	switch NormalizeDeliveryProfile(value) {
	case DeliveryProfileConservativeReview:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileConservativeReview,
			Label:                        "Conservative review mode",
			KanbanMode:                   "read_only",
			AutoPromoteEnabled:           false,
			AutoPromoteQuietSeconds:      600,
			GateRequireAutomatedReview:   true,
			DependencyAutoUnblockEnabled: false,
			MergingConcurrency:           1,
		}, true
	case DeliveryProfileAutonomousDelivery:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileAutonomousDelivery,
			Label:                        "Autonomous delivery mode",
			KanbanMode:                   "integration",
			AutoPromoteEnabled:           true,
			AutoPromoteQuietSeconds:      0,
			GateRequireAutomatedReview:   false,
			DependencyAutoUnblockEnabled: true,
			MergingConcurrency:           1,
		}, true
	default:
		return DeliveryProfileSettings{}, false
	}
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
