package config

// isHistoricalDecisionReason accepts previously valid staleness settings during
// startup and reload. These values must not return to emitted reasons or defaults.
func isHistoricalDecisionReason(reason string) bool {
	switch reason {
	case "merge_fairness_head_reserved", "selected_project_waiting", "reserved_for_higher_priority_state", "reserved_for_higher_priority_project":
		return true
	default:
		return false
	}
}
