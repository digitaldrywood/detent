package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	deliverableRecoveryNeedsHumanReason = "deliverable_recovery_needs_human_attention"
	noCommitsToDeliverReason            = "no_commits_to_deliver"
	deliverableRecoveryLookupAttempts   = 3
	deliverableRecoveryLookupBackoff    = 250 * time.Millisecond
)

type deliverableRecoveryLookupResult struct {
	Branch               string
	Repository           string
	HeadSHA              string
	HydrationState       string
	LookupResult         string
	Attempts             int
	PullRequest          *connector.PullRequest
	CommitsAhead         int
	RemoteBranchExists   bool
	DeliveryStateChecked bool
}

func (r deliverableRecoveryLookupResult) reconciles() bool {
	if r.PullRequest == nil {
		return false
	}
	state := normalizePullRequestState(r.PullRequest.State)
	return state == "open" || state == "merged"
}

func (o *Orchestrator) lookupDeliverableRecovery(
	ctx context.Context,
	running Running,
	recoveryErr *runpkg.DeliverableRecoveryError,
) deliverableRecoveryLookupResult {
	result := deliverableRecoveryLookupResult{
		Branch:               deliverableRecoveryBranch(recoveryErr, running),
		Repository:           pullRequestRepository(running.Issue),
		HeadSHA:              strings.TrimSpace(running.DiffStats.HeadSHA),
		HydrationState:       deliverableRecoveryHydrationState(running.Issue.PullRequest),
		CommitsAhead:         running.DiffStats.CommitsAhead,
		RemoteBranchExists:   running.DiffStats.RemoteBranchExists,
		DeliveryStateChecked: running.DiffStats.DeliveryStateChecked,
	}
	if !result.DeliveryStateChecked {
		result.LookupResult = "PR lookup skipped: delivery state check unavailable"
		return result
	}
	if result.CommitsAhead == 0 {
		result.LookupResult = "PR lookup skipped: no local commits ahead"
		return result
	}
	if !result.RemoteBranchExists {
		result.LookupResult = "PR lookup skipped: remote branch is missing"
		return result
	}
	if result.Repository == "" {
		result.LookupResult = "PR lookup unavailable: repository is unknown"
		return result
	}
	if result.HeadSHA == "" {
		result.LookupResult = "PR lookup unavailable: current workspace head SHA is unknown"
		return result
	}
	lookup, ok := o.connector.(connector.PullRequestHeadLookup)
	if !ok {
		result.LookupResult = "PR lookup unavailable: connector does not support exact-head lookup"
		return result
	}

	var (
		lookupErr error
		notFound  bool
	)
	for attempt := 1; attempt <= deliverableRecoveryLookupAttempts; attempt++ {
		result.Attempts = attempt
		pullRequest, found, err := lookup.LookupPullRequestByHead(ctx, result.Repository, result.Branch, result.HeadSHA)
		if err == nil {
			lookupErr = nil
			if !found {
				notFound = true
			} else {
				fresh := pullRequest
				result.PullRequest = &fresh
				if strings.TrimSpace(fresh.BranchName) != result.Branch || strings.TrimSpace(fresh.HeadSHA) != result.HeadSHA {
					result.LookupResult = fmt.Sprintf(
						"no exact-head pull request; lookup returned PR #%d branch %q head %q",
						fresh.Number,
						strings.TrimSpace(fresh.BranchName),
						strings.TrimSpace(fresh.HeadSHA),
					)
					result.PullRequest = nil
					return result
				}
				state := normalizePullRequestState(fresh.State)
				switch state {
				case "open", "merged":
					result.LookupResult = fmt.Sprintf("exact-head pull request #%d is %s", fresh.Number, state)
				case "closed":
					result.LookupResult = fmt.Sprintf("exact-head pull request #%d is closed without merge", fresh.Number)
				default:
					result.LookupResult = fmt.Sprintf("exact-head pull request #%d has state %q", fresh.Number, strings.TrimSpace(fresh.State))
				}
				return result
			}
		} else {
			lookupErr = err
			notFound = false
		}
		if attempt == deliverableRecoveryLookupAttempts {
			break
		}
		wait := o.deliverableRecoveryWait
		if wait == nil {
			wait = waitForDispatchBackoff
		}
		if !wait(ctx, deliverableRecoveryLookupBackoff*time.Duration(1<<(attempt-1))) {
			lookupErr = ctx.Err()
			notFound = false
			break
		}
	}
	if notFound {
		result.LookupResult = "no exact-head pull request after " + strconv.Itoa(result.Attempts) + " attempts"
		return result
	}
	result.LookupResult = "PR lookup unavailable after " + strconv.Itoa(result.Attempts) + " attempts"
	if lookupErr != nil {
		result.LookupResult += ": " + o.operatorText(lookupErr.Error())
	}
	return result
}

func deliverableRecoveryHydrationState(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return "none at dispatch"
	}
	return fmt.Sprintf(
		"cached at dispatch: PR #%d state=%q branch=%q head=%q",
		pullRequest.Number,
		strings.TrimSpace(pullRequest.State),
		strings.TrimSpace(pullRequest.BranchName),
		strings.TrimSpace(pullRequest.HeadSHA),
	)
}

func deliverableRecoveryIssue(running Running, lookup deliverableRecoveryLookupResult) connector.Issue {
	issue := cloneIssue(running.Issue)
	if lookup.PullRequest == nil {
		return issue
	}
	pullRequest := *lookup.PullRequest
	issue.PullRequest = &pullRequest
	issue.PRRepository = lookup.Repository
	number := pullRequest.Number
	issue.PRNumber = &number
	return issue
}

func (o *Orchestrator) blockDeliverableRecoveryFailure(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	lookup deliverableRecoveryLookupResult,
) bool {
	var recoveryErr *runpkg.DeliverableRecoveryError
	if state == nil || !errors.As(event.Err, &recoveryErr) || recoveryErr == nil {
		return false
	}
	branch := lookup.Branch
	if branch == "" {
		branch = deliverableRecoveryBranch(recoveryErr, running)
		lookup.Branch = branch
	}
	reason, humanAction := deliverableRecoveryParkReason(lookup)
	issue := cloneIssue(running.Issue)
	if lookup.PullRequest != nil {
		issue = deliverableRecoveryIssue(running, lookup)
	} else if strings.HasPrefix(strings.TrimSpace(lookup.LookupResult), "no exact-head pull request") {
		issue.PullRequest = nil
		issue.PRNumber = nil
	}
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		running.Mode,
		reason,
		blockedRecoveryPredicateManaged,
		autoPromoteReworkState,
		running.DiffStats,
	)
	metadata.BlockedRecovery.Owner = blockedRecoveryOwnerHuman
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, blockedStatusState, event.CompletedAt, reason, metadata); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"deliverable recovery block transition failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"workspace_branch", branch,
				"error", err,
			)
		}
		return false
	}
	issue.State = blockedStatusState
	issue.BlockerReason = reason
	blockedAt := event.CompletedAt.UTC()
	issue.StageUpdatedAt = &blockedAt
	if o.connector != nil {
		comment := "Pull request delivery remains unrecoverable and needs human attention.\n\n" +
			"- reason: `" + reason + "`\n" +
			"- branch: `" + branch + "`\n" +
			"- workspace head: `" + lookup.HeadSHA + "`\n" +
			"- local commits ahead: " + deliverableRecoveryCommitsAheadText(lookup) + "\n" +
			"- remote branch exists: " + deliverableRecoveryRemoteBranchText(lookup) + "\n" +
			"- hydration state: " + lookup.HydrationState + "\n" +
			"- lookup result: " + lookup.LookupResult + "\n" +
			"- lookup attempts: " + strconv.Itoa(lookup.Attempts) + "\n" +
			"- failing command: `" + deliverableRecoveryCommand(recoveryErr) + "`\n" +
			"- recovery: " + humanAction + "\n" +
			"- error: " + o.operatorText(recoveryErr.Error())
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn("deliverable recovery block comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	delete(state.InstantFailures, issue.ID)
	delete(state.RepeatedFailures, issue.ID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issue.ID] = Blocked{
		Issue:          issue,
		Reason:         reason,
		RecoveryReason: humanAction,
		RecoveryTarget: autoPromoteReworkState,
		BlockedAt:      event.CompletedAt,
		Source:         BlockedSourceProjectStatus,
		Recovery:       metadata.BlockedRecovery,
	}
	reasonCode := deliverableRecoveryReasonCode(lookup)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   reasonCode,
		Message: "parked " + issueLabel(issue) + ": " + reason,
	})
	telemetry.LogLifecycle(o.logger, slog.LevelError, telemetry.LifecycleSafetyControl, reasonCode, o.runningLifecycleCorrelation(issue, running),
		"workspace_branch", branch,
		"local_commits_ahead", lookup.CommitsAhead,
		"remote_branch_exists", lookup.RemoteBranchExists,
		"delivery_state_checked", lookup.DeliveryStateChecked,
		"deliverable_command", deliverableRecoveryCommand(recoveryErr),
		"error", recoveryErr,
	)
	return true
}

func deliverableRecoveryParkReason(lookup deliverableRecoveryLookupResult) (string, string) {
	branch := strings.TrimSpace(lookup.Branch)
	lookupResult := strings.TrimSpace(lookup.LookupResult)
	base := deliverableRecoveryNeedsHumanReason + ": pushed branch " + branch + " has no recoverable pull request"
	humanAction := "open or adopt a pull request manually for pushed branch " + branch + ", then move the issue to Rework"
	if lookup.DeliveryStateChecked && lookup.CommitsAhead == 0 {
		base = noCommitsToDeliverReason + ": branch " + branch + " has no local commits ahead"
		humanAction = "return the issue to Todo when implementation work is ready to resume"
	} else if lookup.DeliveryStateChecked && !lookup.RemoteBranchExists {
		base = deliverableRecoveryNeedsHumanReason + ": remote branch is missing for branch " + branch
		humanAction = "push the local branch, then move the issue to Rework"
	} else if !lookup.DeliveryStateChecked {
		base = deliverableRecoveryNeedsHumanReason + ": delivery state check unavailable for branch " + branch
		humanAction = "restore workspace delivery inspection, then move the issue to Rework"
	} else if strings.HasPrefix(lookupResult, "PR lookup unavailable") {
		base = deliverableRecoveryNeedsHumanReason + ": PR lookup unavailable for pushed branch " + branch
		humanAction = "restore pull request lookup availability, then move the issue to Rework"
	} else if lookup.PullRequest != nil && normalizePullRequestState(lookup.PullRequest.State) == "closed" {
		base = deliverableRecoveryNeedsHumanReason + ": pushed branch " + branch + " has an exact-head pull request closed without merge"
		humanAction = "reopen the exact-head pull request or open a replacement, then move the issue to Rework"
	}
	return base + " (hydration state: " + lookup.HydrationState + "; lookup result: " + lookupResult + ")", humanAction
}

func deliverableRecoveryReasonCode(lookup deliverableRecoveryLookupResult) string {
	if lookup.DeliveryStateChecked && lookup.CommitsAhead == 0 {
		return noCommitsToDeliverReason
	}
	return deliverableRecoveryNeedsHumanReason
}

func deliverableRecoveryCommitsAheadText(lookup deliverableRecoveryLookupResult) string {
	if !lookup.DeliveryStateChecked {
		return "unavailable"
	}
	return strconv.Itoa(lookup.CommitsAhead)
}

func deliverableRecoveryRemoteBranchText(lookup deliverableRecoveryLookupResult) string {
	if !lookup.DeliveryStateChecked {
		return "unavailable"
	}
	return strconv.FormatBool(lookup.RemoteBranchExists)
}

func deliverableRecoveryBranch(recoveryErr *runpkg.DeliverableRecoveryError, running Running) string {
	if recoveryErr != nil {
		if branch := strings.TrimSpace(recoveryErr.Branch); branch != "" {
			return branch
		}
	}
	if branch := strings.TrimSpace(running.Issue.BranchName); branch != "" {
		return branch
	}
	return "unknown"
}

func deliverableRecoveryCommand(recoveryErr *runpkg.DeliverableRecoveryError) string {
	var deliverableErr *runpkg.DeliverableCommandError
	if recoveryErr != nil && errors.As(recoveryErr, &deliverableErr) && deliverableErr != nil {
		if command := strings.TrimSpace(deliverableErr.Operation); command != "" {
			return command
		}
	}
	return "pull request creation"
}
