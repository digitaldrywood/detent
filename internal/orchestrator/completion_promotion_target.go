package orchestrator

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

// A completed attempt is only ever moved out of its active lane by the
// orchestrator, and the lane it is moved to is named by configuration
// (AutoPromote.SourceState, "Human Review" by default). A project whose
// workflow has no such lane therefore leaves the item where it is --
// non-terminal, dispatchable, and claimable again on the next tick -- so a
// successful attempt is followed by another attempt, forever. That is
// operations.md section 7, "An item in an active state is re-dispatched
// forever".
//
// The promotion target is resolved against the project's own workflow before
// it is used: the configured lane when the project has it, otherwise the
// project's own review lane, otherwise its first terminal lane. The fallback
// is deterministic and is logged once per item, because a promotion into a
// lane the operator did not name is a fact worth reading in the log.

// CompletedReviewReady reports whether a completed item may leave its active
// lane at all, and the readiness fact that is missing when it may not. It is
// the rule the completion tick applies, exported beside CompletedReviewTarget
// because the two halves of a promotion are answered separately: this one
// decides whether to promote, and CompletedReviewTarget decides where to.
//
// The issue must already carry its tracker's change review surface, which
// ChangeReviewHydrator is what reports; without it the rule falls back to the
// pull request, which is all it ever had.
func CompletedReviewReady(issue connector.Issue, gateCfg gate.Config) (bool, string) {
	return completedActiveIssueReadyForReview(issue, gateRequiresPullRequest(gateCfg), false)
}

// CompletedReviewTarget reports the lane a completed item is promoted into on
// a project whose workflow is known: the configured review state when the
// project has it, otherwise the project's own review lane, otherwise its first
// terminal lane. An empty result means the project offers nowhere to promote
// into and the item is left where it is.
func CompletedReviewTarget(states []connector.WorkflowState, configuredReviewState string) string {
	if workflowStateNamed(states, configuredReviewState) {
		return strings.TrimSpace(configuredReviewState)
	}
	return completedActiveReviewFallbackState(states)
}

// completedActiveReviewFallbackState picks the lane a completed item is
// promoted into when the configured review state is not one of the project's
// own.
//
// A review lane -- non-terminal, not dispatchable, not operator-only -- is
// preferred, because that is exactly what the configured review state means:
// work parks there for a person and no runner claims it. The project may
// spell it differently from the configuration, so it is recognized by name.
//
// Otherwise the project's first terminal lane, which is the same rule
// closeCoordinatorItem and the workspace close already apply when they retire
// an item the issue lane must stop offering.
func completedActiveReviewFallbackState(states []connector.WorkflowState) string {
	for _, state := range states {
		if state.Terminal || state.Dispatchable || state.OperatorOnly {
			continue
		}
		if strings.Contains(normalizeState(state.Name), "review") {
			return strings.TrimSpace(state.Name)
		}
	}
	for _, state := range states {
		if state.Terminal {
			return strings.TrimSpace(state.Name)
		}
	}
	return ""
}

// workflowStateNamed reports whether the project's workflow has a lane with
// this name, comparing the way every other state comparison here does.
func workflowStateNamed(states []connector.WorkflowState, name string) bool {
	target := normalizeState(name)
	if target == "" {
		return false
	}
	for _, state := range states {
		if normalizeState(state.Name) == target {
			return true
		}
	}
	return false
}

// tickWorkflowStates returns a reader that resolves the project's lanes at
// most once, on its first call. Reading them costs a tracker request, so a
// tick that never reaches a promotion never pays for one, and a tick with ten
// promotions pays once.
func (o *Orchestrator) tickWorkflowStates(ctx context.Context) func() ([]connector.WorkflowState, bool) {
	var (
		states []connector.WorkflowState
		known  bool
		read   bool
	)
	return func() ([]connector.WorkflowState, bool) {
		if !read {
			read = true
			states, known = o.projectWorkflowStates(ctx)
		}
		return states, known
	}
}

// projectWorkflowStates reports the project's lanes, or false when the
// connector cannot answer. A connector with no workflow to report leaves the
// configured state names as the only authority, which is what every connector
// did before the fallback existed.
func (o *Orchestrator) projectWorkflowStates(ctx context.Context) ([]connector.WorkflowState, bool) {
	lister, ok := o.connector.(connector.WorkflowStateLister)
	if !ok {
		return nil, false
	}
	states, err := lister.ListWorkflowStates(ctx)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("read project workflow states failed", "error", err)
		}
		return nil, false
	}
	if len(states) == 0 {
		return nil, false
	}
	return states, true
}

// resolveCompletedActiveReviewTarget maps the configured promotion target onto
// a lane the project actually has. It reports the lane to move to and whether
// that lane is a fallback rather than the configured one; an empty lane means
// the project offers nowhere to promote into and the item is left alone.
func (o *Orchestrator) resolveCompletedActiveReviewTarget(
	states []connector.WorkflowState,
	known bool,
	issue connector.Issue,
	targetState string,
) (string, bool) {
	if !known || workflowStateNamed(states, targetState) {
		return targetState, false
	}
	fallback := CompletedReviewTarget(states, targetState)
	o.logCompletedActiveReviewFallback(issue, targetState, fallback)
	if fallback == "" {
		return "", false
	}
	return fallback, true
}

// forgetPromotionFallbackLogs drops the "logged once" memory for every item
// that is no longer waiting to be promoted, the way the hydration starvation
// streaks are pruned against their own current set. An item that reached its
// lane is done with, and an item that comes back to be promoted again is a new
// promotion worth a new line.
func (o *Orchestrator) forgetPromotionFallbackLogs(state *State) {
	for _, logged := range []map[string]string{o.promotionFallbackLogged, o.promotionNotReadyLogged} {
		for issueID := range logged {
			if _, pending := state.Completed[issueID]; !pending {
				delete(logged, issueID)
			}
		}
	}
}

// hydrateCompletedChangeReview asks a tracker that reviews changes of its own
// what it holds for this item. The issue the caller has was read when the
// attempt was dispatched, so the change the attempt produced is newer than it;
// without this the promotion judges a native item on facts that predate the
// work it is promoting.
//
// A connector that cannot answer, or one that fails to, leaves the issue as it
// was. The promotion then falls back to the pull request, finds none, and says
// so once rather than skipping in silence.
func (o *Orchestrator) hydrateCompletedChangeReview(ctx context.Context, issue connector.Issue) connector.Issue {
	hydrator, ok := o.connector.(connector.ChangeReviewHydrator)
	if !ok {
		return issue
	}
	hydrated, err := hydrator.HydrateChangeReview(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"read issue change review failed",
				"issue_id", strings.TrimSpace(issue.ID),
				"identifier", issue.Identifier,
				"error", err,
			)
		}
		return issue
	}
	return hydrated
}

// logCompletedActiveReviewNotReady names the readiness fact a completed item
// is missing, once per item.
//
// The silent continue this replaces is what hid the whole native promotion for
// seven dogfood runs: the item stayed in its active lane, nothing was claimed,
// nothing was logged, and the only visible symptom was a board row that looked
// like outstanding work. Once per item is once per promotion, for the same
// reason the lane substitution is logged once.
func (o *Orchestrator) logCompletedActiveReviewNotReady(issue connector.Issue, missing string) {
	issueID := strings.TrimSpace(issue.ID)
	if o.logger == nil || issueID == "" || missing == "" {
		return
	}
	if o.promotionNotReadyLogged == nil {
		o.promotionNotReadyLogged = map[string]string{}
	}
	if previous, logged := o.promotionNotReadyLogged[issueID]; logged && previous == missing {
		return
	}
	o.promotionNotReadyLogged[issueID] = missing
	fields := []any{
		"issue_id", issueID,
		"identifier", issue.Identifier,
		"state", issue.State,
		"missing", missing,
	}
	if review := issue.ChangeReview; review != nil {
		fields = append(fields,
			"change_review_provider", review.Provider,
			"pull_requests_available", review.PullRequestsAvailable,
			"change_revision", review.ChangeRevision,
			"issue_revision", review.Revision,
		)
	}
	o.logger.Warn("completed issue not ready for promotion", fields...)
}

// logCompletedActiveReviewFallback reports the substitution once per item. An
// item is promoted once, so once per item is once per promotion; repeating it
// on every tick of a project whose workflow simply does not have the
// configured lane would bury everything else.
func (o *Orchestrator) logCompletedActiveReviewFallback(issue connector.Issue, configured string, fallback string) {
	issueID := strings.TrimSpace(issue.ID)
	if o.logger == nil || issueID == "" {
		return
	}
	if o.promotionFallbackLogged == nil {
		o.promotionFallbackLogged = map[string]string{}
	}
	if previous, logged := o.promotionFallbackLogged[issueID]; logged && previous == fallback {
		return
	}
	o.promotionFallbackLogged[issueID] = fallback
	if fallback == "" {
		o.logger.Warn(
			"completed issue has no promotion lane",
			"issue_id", issueID,
			"identifier", issue.Identifier,
			"state", issue.State,
			"configured_state", configured,
		)
		return
	}
	o.logger.Info(
		"completed issue promotion lane substituted",
		"issue_id", issueID,
		"identifier", issue.Identifier,
		"state", issue.State,
		"configured_state", configured,
		"target_state", fallback,
	)
}
