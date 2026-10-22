package connector

import "strings"

// PullRequestConflicts recognizes the conflict spellings returned by trackers.
// Unknown or absent mergeability is not evidence of a conflict or merge readiness.
func PullRequestConflicts(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "dirty", "conflicting":
		return true
	default:
		return false
	}
}

// PullRequestConflictCleared reports progress when a tracker stops reporting a
// known conflict. Unknown includes recomputation after a push; it does not grant
// permission to merge. An absent state is missing evidence, not progress.
func PullRequestConflictCleared(before, after string) bool {
	if !PullRequestConflicts(before) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(after)) {
	case "clean", "unstable", "has_hooks", "behind", "blocked", "draft", "unknown", "mergeable":
		return true
	default:
		return false
	}
}
