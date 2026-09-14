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
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/issueorigin"
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
	CreatedPullRequest   bool
	CreateError          error
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

func (o *Orchestrator) createDeliverableRecoveryPullRequest(
	ctx context.Context,
	running Running,
	result deliverableRecoveryLookupResult,
) deliverableRecoveryLookupResult {
	if result.PullRequest != nil || !result.DeliveryStateChecked || result.CommitsAhead <= 0 || !result.RemoteBranchExists ||
		!strings.HasPrefix(strings.TrimSpace(result.LookupResult), "no exact-head pull request") {
		return result
	}
	creator, ok := o.connector.(connector.PullRequestDraftCreator)
	if !ok {
		result.LookupResult = "draft pull request creation unavailable: connector does not support it"
		return result
	}
	pullRequest, err := creator.CreateDraftPullRequest(
		ctx,
		result.Repository,
		result.Branch,
		running.Issue.Title,
		deliverableRecoveryPullRequestBody(running.Issue, result.Branch, result.HeadSHA),
	)
	if err != nil {
		result.CreateError = err
		result.LookupResult += "; draft pull request creation failed: " + o.operatorText(err.Error())
		return result
	}
	if normalizePullRequestState(pullRequest.State) != "open" || !pullRequest.Draft ||
		strings.TrimSpace(pullRequest.BranchName) != result.Branch || strings.TrimSpace(pullRequest.HeadSHA) != result.HeadSHA {
		result.LookupResult += fmt.Sprintf(
			"; draft pull request creation returned inconsistent PR #%d state=%q draft=%t branch=%q head=%q",
			pullRequest.Number,
			strings.TrimSpace(pullRequest.State),
			pullRequest.Draft,
			strings.TrimSpace(pullRequest.BranchName),
			strings.TrimSpace(pullRequest.HeadSHA),
		)
		return result
	}
	result.PullRequest = &pullRequest
	result.CreatedPullRequest = true
	result.LookupResult = fmt.Sprintf("draft pull request #%d opened for exact current head", pullRequest.Number)
	return result
}

func (o *Orchestrator) deliverableRecoveryInfrastructureError(running Running, err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if availabilityErr, ok := forgeavailability.As(err); ok && availabilityErr != nil {
		return err, true
	}
	if availabilityErr, ok := connector.AsTrackerAvailability(err); ok && availabilityErr != nil {
		return err, true
	}
	if isGitHubRESTBudgetHeadroomError(err) {
		return err, true
	}
	const operation = "create_pull_request"
	class, unavailable := forgeavailability.Classify(operation, err.Error())
	configurationErr := &runpkg.DeliverableCommandError{
		OperationClass: "pull_request",
		Operation:      operation,
		Status:         "failed",
		Message:        err.Error(),
	}
	if unavailable {
		return forgeavailability.NewError(forgeavailability.Scope{
			Host:      forgeHostForIssue(running.Issue, o.cfg.ForgeHost),
			Operation: operation,
		}, class, err), true
	}
	if connector.IsRetryable(err) || runpkg.IsDeliverableConfigurationError(configurationErr) {
		return err, true
	}
	return err, false
}

func deliverableRecoveryPullRequestBody(issue connector.Issue, branch string, headSHA string) string {
	identifier := strings.TrimSpace(issue.Identifier)
	body := "Detent recovered this deliverable after the worker pushed its branch without opening a pull request."
	if identifier != "" {
		body += "\n\nFixes " + identifier
	}
	return issueorigin.Stamp(body, issueorigin.Origin{
		Kind:        "worker",
		Source:      "deliverable-recovery/" + identifier,
		Fingerprint: issueorigin.Fingerprint(strings.Join([]string{"deliverable-recovery", identifier, strings.TrimSpace(branch), strings.TrimSpace(headSHA)}, ":")),
	})
}

func deliverableRecoveryMachineOwned(lookup deliverableRecoveryLookupResult) bool {
	if !lookup.DeliveryStateChecked || lookup.CommitsAhead <= 0 || lookup.PullRequest != nil {
		return false
	}
	return !lookup.RemoteBranchExists || strings.HasPrefix(strings.TrimSpace(lookup.LookupResult), "no exact-head pull request")
}

func (o *Orchestrator) returnMissingDeliverableBranchToRework(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	lookup deliverableRecoveryLookupResult,
	at time.Time,
) connector.Issue {
	if !lookup.DeliveryStateChecked || lookup.CommitsAhead <= 0 || lookup.RemoteBranchExists {
		return issue
	}
	target := normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState
	if strings.TrimSpace(target) == "" {
		target = autoPromoteReworkState
	}
	if err := o.updateIssueState(ctx, state, issue, target, at, terminalAttemptWithoutWorkProductReason); err != nil {
		if o.logger != nil {
			o.logger.Warn("deliverable recovery rework transition failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return issue
	}
	issue.State = target
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      at,
		Event:   "terminal_attempt_retry_demoted",
		Message: "moved " + issueLabel(issue) + " to " + target + " because the pushed branch is no longer available",
	})
	return issue
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
	reasonCode := deliverableRecoveryReasonCode(lookup)
	metadata.BlockedRecovery.HoldReason = reasonCode
	metadata.BlockedRecovery.OperatorRemedy = humanAction
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
		Issue:                issue,
		Reason:               reason,
		RecoveryAction:       "hold",
		RecoveryReason:       reasonCode,
		RecoveryRemedy:       humanAction,
		RecoveryTarget:       autoPromoteReworkState,
		RecoveryReachability: blockedRecoveryReachability("hold"),
		NeedsHumanAttention:  true,
		BlockedAt:            event.CompletedAt,
		Source:               BlockedSourceProjectStatus,
		Recovery:             metadata.BlockedRecovery,
	}
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

func (o *Orchestrator) blockHumanOwnedWorkerFailure(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	cause string,
	detail string,
	humanAction string,
	eventName string,
	eventAttrs ...any,
) bool {
	if state == nil || event.Err == nil || strings.TrimSpace(cause) == "" {
		return false
	}
	issue := cloneIssue(running.Issue)
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		running.Mode,
		cause,
		blockedRecoveryPredicateManaged,
		autoPromoteReworkState,
		running.DiffStats,
	)
	metadata.BlockedRecovery.Owner = blockedRecoveryOwnerHuman
	metadata.BlockedRecovery.HoldReason = cause
	metadata.BlockedRecovery.OperatorRemedy = humanAction
	metadata.BlockedRecovery.WorkAttemptID = running.WorkAttemptID
	metadata.BlockedRecovery.AttemptNumber = running.Attempt
	metadata.BlockedRecovery.AttemptError = detail
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, blockedStatusState, event.CompletedAt, cause, metadata); err != nil {
		if o.logger != nil {
			o.logger.Warn(eventName+" state transition failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return false
	}
	issue.State = blockedStatusState
	issue.BlockerReason = cause
	blockedAt := event.CompletedAt.UTC()
	issue.StageUpdatedAt = &blockedAt
	if o.connector != nil {
		comment := "Detent parked this issue after a non-retryable worker safety failure.\n\n" +
			"- cause: `" + cause + "`\n" +
			"- detail: " + detail + "\n" +
			"- human_action: " + humanAction
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn(eventName+" comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.Warn(eventName+" claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
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
		Issue:               issue,
		Reason:              cause,
		AttemptError:        detail,
		WorkAttemptID:       running.WorkAttemptID,
		RecoveryReason:      "human acknowledgement required",
		RecoveryTarget:      autoPromoteReworkState,
		RecoveryRemedy:      humanAction,
		NeedsHumanAttention: true,
		BlockedAt:           event.CompletedAt,
		Source:              BlockedSourceProjectStatus,
		Recovery:            metadata.BlockedRecovery,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   eventName,
		Message: "parked " + issueLabel(issue) + ": " + detail,
	})
	attrs := []any{
		"cause", cause,
		"human_action", humanAction,
		"error", event.Err,
	}
	attrs = append(attrs, eventAttrs...)
	telemetry.LogLifecycleMessage(o.logger, slog.LevelError, telemetry.LifecycleSafetyControl, eventName, detail, o.runningLifecycleCorrelation(issue, running), attrs...)
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
