package connector

import (
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/workpad"
)

// WithNativeWorkpadAuthority excludes issue dependency predicates absent from
// the current native relation list. Other predicates and human actions retain
// their meaning; the worker's recorded status is never rewritten.
func (issue Issue) WithNativeWorkpadAuthority() Issue {
	if issue.DependencySource != BlockedRefSourceNative || issue.WorkpadSignal == nil {
		return issue
	}
	signal := workpad.CloneSignal(issue.WorkpadSignal)
	signal.Blockers = nil
	issue.DependencyNotes = slices.Clone(issue.DependencyNotes)
	for _, blocker := range issue.WorkpadSignal.Blockers {
		identifier := issue.IgnoredNativeWorkpadDependency(blocker)
		if identifier == "" {
			signal.Blockers = append(signal.Blockers, blocker)
			continue
		}
		note := identifier + ": workpad dependency ignored: native relation absent"
		if !slices.Contains(issue.DependencyNotes, note) {
			issue.DependencyNotes = append(issue.DependencyNotes, note)
		}
	}
	issue.WorkpadSignal = signal
	issue.BlockerReason = workpad.Reason(signal)
	return issue
}

// IgnoredNativeWorkpadDependency names a recorded dependency absent from the
// current native list. Recovery uses that same source decision as cleared evidence.
func (issue Issue) IgnoredNativeWorkpadDependency(blocker workpad.Blocker) string {
	if issue.DependencySource != BlockedRefSourceNative || blocker.Predicate != nil && blocker.Predicate.Type != workpad.PredicateIssueState {
		return ""
	}
	identifier := ""
	if blocker.Predicate != nil {
		identifier = blocker.Predicate.Identifier
	}
	if identifier == "" {
		identifier = blocker.Identifier
	}
	if identifier == "" {
		identifier = blocker.Ref
	}
	repo, _, _ := strings.Cut(issue.Identifier, "#")
	identifier, err := workpad.ParseRef(identifier, repo)
	if err != nil {
		return ""
	}
	if slices.ContainsFunc(issue.BlockedBy, func(ref BlockedRef) bool { return strings.EqualFold(strings.TrimSpace(ref.Identifier), identifier) }) {
		return ""
	}
	return identifier
}
