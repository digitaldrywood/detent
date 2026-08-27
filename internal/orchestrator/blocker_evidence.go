package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	blockerEvidenceStatusHolds            = "holds"
	blockerEvidenceStatusCleared          = "cleared"
	blockerEvidenceStatusUnverifiable     = "unverifiable"
	workflowActionRecordedBlockerRecovery = "recorded_blocker_recovery"
)

type blockerPredicateContext struct {
	issue           connector.Issue
	state           *State
	now             time.Time
	references      map[string]connector.Issue
	hydrated        map[string]connector.Issue
	hydrationErrors map[string]error
}

type blockerPredicateEvaluator func(context.Context, *Orchestrator, *blockerPredicateContext, workpad.Predicate) (string, string)

var blockerPredicateRegistry = map[string]blockerPredicateEvaluator{
	workpad.PredicateIssueState:        evaluateIssueStatePredicate,
	workpad.PredicatePullRequestState:  evaluatePullRequestStatePredicate,
	workpad.PredicateCheckPresence:     evaluateCheckPresencePredicate,
	workpad.PredicateBudgetCapacity:    evaluateBudgetCapacityPredicate,
	workpad.PredicateConfigFingerprint: evaluateConfigFingerprintPredicate,
}

type recordedBlockerEvaluation struct {
	Evidence     []telemetry.BlockerEvidence
	Found        bool
	Holds        bool
	Unverifiable bool
	HumanOwned   bool
}

func (o *Orchestrator) evaluateRecordedBlockers(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	resolvedReferences map[string]connector.Issue,
	now time.Time,
) recordedBlockerEvaluation {
	signal := issue.WorkpadSignal
	if signal == nil {
		return recordedBlockerEvaluation{}
	}
	if signal.Source == workpad.SourceStructured && strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return recordedBlockerEvaluation{}
	}
	result := recordedBlockerEvaluation{}
	recordedAt := recordedBlockerTime(state, issue, signal)
	if signal.Invalid != nil {
		result.Found = true
		result.Unverifiable = true
		result.HumanOwned = true
		result.Evidence = append(result.Evidence, newBlockerEvidence(
			"invalid",
			workpad.BlockerOwnerHuman,
			blockerEvidenceStatusUnverifiable,
			"",
			signal.Invalid.Message,
			"the structured blocker could not be parsed",
			recordedAt,
			nil,
			"",
			now,
		))
		return result
	}

	predicateContext := &blockerPredicateContext{
		issue:           issue,
		state:           state,
		now:             now,
		references:      o.resolveBlockerPredicateReferences(ctx, issue, signal.Blockers, resolvedReferences),
		hydrated:        map[string]connector.Issue{},
		hydrationErrors: map[string]error{},
	}
	for _, blocker := range signal.Blockers {
		if blocker.Predicate == nil && !blocker.Unverifiable {
			continue
		}
		result.Found = true
		evidence := o.evaluateRecordedBlocker(ctx, predicateContext, blocker, recordedAt)
		result.Evidence = append(result.Evidence, evidence)
		result.Holds = result.Holds || evidence.Status == blockerEvidenceStatusHolds
		result.Unverifiable = result.Unverifiable || evidence.Unverifiable
		result.HumanOwned = result.HumanOwned || evidence.Owner == workpad.BlockerOwnerHuman && evidence.Status != blockerEvidenceStatusCleared
	}
	if humanAction := strings.TrimSpace(signal.HumanAction); humanAction != "" {
		result.Found = true
		result.Unverifiable = true
		result.HumanOwned = true
		result.Evidence = append(result.Evidence, newBlockerEvidence(
			"free_text",
			workpad.BlockerOwnerHuman,
			blockerEvidenceStatusUnverifiable,
			"",
			humanAction,
			"human_action has no live predicate",
			recordedAt,
			nil,
			"",
			now,
		))
	}
	return result
}

func (o *Orchestrator) evaluateRecordedBlocker(
	ctx context.Context,
	predicateContext *blockerPredicateContext,
	blocker workpad.Blocker,
	recordedAt *time.Time,
) telemetry.BlockerEvidence {
	owner := strings.TrimSpace(blocker.Owner)
	if owner == "" {
		owner = workpad.BlockerOwnerHuman
		if blocker.Predicate != nil {
			owner = workpad.BlockerOwnerOrchestrator
		}
	}
	predicate := blocker.Predicate
	if predicate == nil || blocker.Unverifiable {
		return newBlockerEvidence(
			"free_text",
			owner,
			blockerEvidenceStatusUnverifiable,
			firstNonBlank(blocker.Identifier, blocker.Ref),
			blocker.Reason,
			"free-text blocker has no live predicate",
			recordedAt,
			blocker.ExpiresAt,
			blocker.RecheckInterval,
			predicateContext.now,
		)
	}
	evaluator, ok := blockerPredicateRegistry[predicate.Type]
	if !ok {
		return newBlockerEvidence(
			predicate.Type,
			owner,
			blockerEvidenceStatusUnverifiable,
			blockerPredicateReference(*predicate),
			blocker.Reason,
			"predicate evaluator is not registered",
			recordedAt,
			blocker.ExpiresAt,
			blocker.RecheckInterval,
			predicateContext.now,
		)
	}
	status, detail := evaluator(ctx, o, predicateContext, *predicate)
	if status == blockerEvidenceStatusHolds && blocker.ExpiresAt != nil && !predicateContext.now.Before(*blocker.ExpiresAt) {
		status = blockerEvidenceStatusUnverifiable
		detail = "predicate expired while the blocking condition still held"
	}
	return newBlockerEvidence(
		predicate.Type,
		owner,
		status,
		blockerPredicateReference(*predicate),
		blocker.Reason,
		detail,
		recordedAt,
		blocker.ExpiresAt,
		blocker.RecheckInterval,
		predicateContext.now,
	)
}

func newBlockerEvidence(
	predicateType string,
	owner string,
	status string,
	reference string,
	reason string,
	detail string,
	recordedAt *time.Time,
	expiresAt *time.Time,
	recheckInterval string,
	now time.Time,
) telemetry.BlockerEvidence {
	evidence := telemetry.BlockerEvidence{
		Type:            strings.TrimSpace(predicateType),
		Owner:           strings.TrimSpace(owner),
		Status:          strings.TrimSpace(status),
		Reference:       strings.TrimSpace(reference),
		Reason:          strings.TrimSpace(reason),
		Detail:          strings.TrimSpace(detail),
		Unverifiable:    status == blockerEvidenceStatusUnverifiable,
		RecheckInterval: strings.TrimSpace(recheckInterval),
	}
	if recordedAt != nil && !recordedAt.IsZero() {
		value := recordedAt.UTC()
		evidence.RecordedAt = &value
		if now.After(value) {
			evidence.AgeSeconds = int64(now.Sub(value) / time.Second)
		}
	}
	if expiresAt != nil && !expiresAt.IsZero() {
		value := expiresAt.UTC()
		evidence.ExpiresAt = &value
	}
	return evidence
}

func recordedBlockerTime(state *State, issue connector.Issue, signal *workpad.Signal) *time.Time {
	if signal != nil && signal.RecordedAt != nil && !signal.RecordedAt.IsZero() {
		value := signal.RecordedAt.UTC()
		return &value
	}
	if state != nil {
		if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && !blocked.BlockedAt.IsZero() {
			value := blocked.BlockedAt.UTC()
			return &value
		}
	}
	for _, value := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if value != nil && !value.IsZero() {
			cloned := value.UTC()
			return &cloned
		}
	}
	return nil
}

func blockerPredicateReference(predicate workpad.Predicate) string {
	return firstNonBlank(predicate.Identifier, predicate.Ref, predicate.Check, predicate.Resource, predicate.Scope, predicate.Fingerprint)
}

func (o *Orchestrator) resolveBlockerPredicateReferences(
	ctx context.Context,
	issue connector.Issue,
	blockers []workpad.Blocker,
	seed map[string]connector.Issue,
) map[string]connector.Issue {
	resolved := map[string]connector.Issue{}
	for identifier, resolvedIssue := range seed {
		resolved[normalizedIssueIdentifier(identifier)] = resolvedIssue
	}
	self := normalizedIssueIdentifier(issue.Identifier)
	if self != "" {
		resolved[self] = issue
	}
	refs := []connector.BlockedRef{}
	seen := map[string]struct{}{}
	for _, blocker := range blockers {
		if blocker.Predicate == nil {
			continue
		}
		predicate := blocker.Predicate
		identifier := normalizedIssueIdentifier(predicate.Identifier)
		if identifier == "" || identifier == self {
			continue
		}
		if _, ok := resolved[identifier]; ok {
			continue
		}
		if _, ok := seen[identifier]; ok {
			continue
		}
		seen[identifier] = struct{}{}
		refs = append(refs, connector.BlockedRef{Identifier: predicate.Identifier})
	}
	if len(refs) == 0 {
		return resolved
	}
	for _, blocker := range o.resolveDependencyBlockers(ctx, connector.Issue{ID: issue.ID, Identifier: issue.Identifier, BlockedBy: refs}) {
		if !blocker.Resolved {
			continue
		}
		identifier := normalizedIssueIdentifier(blocker.Issue.Identifier)
		if identifier != "" {
			resolved[identifier] = blocker.Issue
		}
	}
	return resolved
}

func blockerPredicateIssue(predicateContext *blockerPredicateContext, predicate workpad.Predicate) (connector.Issue, bool) {
	identifier := normalizedIssueIdentifier(predicate.Identifier)
	if identifier == "" || identifier == normalizedIssueIdentifier(predicateContext.issue.Identifier) {
		return predicateContext.issue, true
	}
	issue, ok := predicateContext.references[identifier]
	return issue, ok
}

func evaluateIssueStatePredicate(
	_ context.Context,
	o *Orchestrator,
	predicateContext *blockerPredicateContext,
	predicate workpad.Predicate,
) (string, string) {
	if normalizedIssueIdentifier(predicate.Identifier) == normalizedIssueIdentifier(predicateContext.issue.Identifier) {
		return blockerEvidenceStatusUnverifiable, "issue_state predicate cannot reference the blocked issue itself"
	}
	issue, ok := blockerPredicateIssue(predicateContext, predicate)
	if !ok {
		return blockerEvidenceStatusUnverifiable, "referenced issue could not be resolved"
	}
	if len(predicate.States) == 0 {
		cfg := normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock)
		blocker := dependencyBlocker{Issue: issue, Resolved: true, Ref: connector.BlockedRef{Identifier: issue.Identifier, State: issue.State}}
		if dependencyBlockerReady(blocker, cfg, o.cfg.TerminalStates) {
			return blockerEvidenceStatusCleared, "referenced issue is terminal or its linked pull request merged"
		}
		return blockerEvidenceStatusHolds, "referenced issue remains non-terminal"
	}
	if issueStateMatches(issue, predicate.States) {
		return blockerEvidenceStatusHolds, "referenced issue state matches the blocking states"
	}
	return blockerEvidenceStatusCleared, "referenced issue state no longer matches the blocking states"
}

func issueStateMatches(issue connector.Issue, states []string) bool {
	observed := map[string]struct{}{normalizeState(issue.State): {}}
	if issue.Closed {
		observed["closed"] = struct{}{}
	} else {
		observed["open"] = struct{}{}
	}
	for _, state := range states {
		if _, ok := observed[normalizeState(state)]; ok {
			return true
		}
	}
	return false
}

func evaluatePullRequestStatePredicate(
	ctx context.Context,
	o *Orchestrator,
	predicateContext *blockerPredicateContext,
	predicate workpad.Predicate,
) (string, string) {
	issue, ok := blockerPredicateIssue(predicateContext, predicate)
	if !ok {
		return blockerEvidenceStatusUnverifiable, "referenced issue could not be resolved"
	}
	issue, err := o.hydrateBlockerPredicatePullRequest(ctx, predicateContext, issue)
	if err != nil {
		return blockerEvidenceStatusUnverifiable, "pull request state could not be refreshed"
	}
	state, verifiable := observedPullRequestState(issue)
	if !verifiable {
		return blockerEvidenceStatusUnverifiable, "pull request hydration is unavailable"
	}
	if tokenInList(state, predicate.States) {
		return blockerEvidenceStatusHolds, "pull request state matches the blocking states"
	}
	return blockerEvidenceStatusCleared, "pull request state no longer matches the blocking states"
}

func observedPullRequestState(issue connector.Issue) (string, bool) {
	if issue.PullRequest == nil {
		return "missing", true
	}
	if pullRequestHydrationUnavailableReason(issue.PullRequest) != "" || pullRequestHydrationDegradedReason(issue.PullRequest) != "" {
		return "", false
	}
	state := normalizePullRequestState(issue.PullRequest.State)
	if state == "" {
		return "", false
	}
	return state, true
}

func evaluateCheckPresencePredicate(
	ctx context.Context,
	o *Orchestrator,
	predicateContext *blockerPredicateContext,
	predicate workpad.Predicate,
) (string, string) {
	issue, ok := blockerPredicateIssue(predicateContext, predicate)
	if !ok {
		return blockerEvidenceStatusUnverifiable, "referenced issue could not be resolved"
	}
	issue, err := o.hydrateBlockerPredicatePullRequest(ctx, predicateContext, issue)
	if err != nil {
		return blockerEvidenceStatusUnverifiable, "pull request checks could not be refreshed"
	}
	if issue.PullRequest != nil && (pullRequestHydrationUnavailableReason(issue.PullRequest) != "" || pullRequestHydrationDegradedReason(issue.PullRequest) != "") {
		return blockerEvidenceStatusUnverifiable, "pull request check hydration is unavailable"
	}
	present, verifiable := pullRequestCheckPresent(issue.PullRequest, predicate.Check)
	if !verifiable {
		return blockerEvidenceStatusUnverifiable, "pull request check inventory is unavailable"
	}
	wantPresent := predicate.Present != nil && *predicate.Present
	if present == wantPresent {
		return blockerEvidenceStatusHolds, fmt.Sprintf("check presence remains %t", present)
	}
	return blockerEvidenceStatusCleared, fmt.Sprintf("check presence changed to %t", present)
}

func (o *Orchestrator) hydrateBlockerPredicatePullRequest(
	ctx context.Context,
	predicateContext *blockerPredicateContext,
	issue connector.Issue,
) (connector.Issue, error) {
	key := normalizedIssueIdentifier(issue.Identifier)
	if hydrated, ok := predicateContext.hydrated[key]; ok {
		return hydrated, nil
	}
	if err, ok := predicateContext.hydrationErrors[key]; ok {
		return issue, err
	}
	if issue.PullRequest == nil && issue.PRNumber == nil {
		predicateContext.hydrated[key] = issue
		return issue, nil
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		err := errors.New("connector does not support pull request hydration")
		predicateContext.hydrationErrors[key] = err
		return issue, err
	}
	hydrated, err := hydrator.HydratePullRequest(ctx, issue)
	if err != nil {
		predicateContext.hydrationErrors[key] = err
		return issue, err
	}
	predicateContext.hydrated[key] = hydrated
	return hydrated, nil
}

func pullRequestCheckPresent(pullRequest *connector.PullRequest, checkName string) (bool, bool) {
	if pullRequest == nil {
		return false, true
	}
	checkName = strings.TrimSpace(checkName)
	for _, check := range pullRequest.Checks {
		if strings.EqualFold(strings.TrimSpace(check.Name), checkName) {
			return true, true
		}
	}
	if len(pullRequest.Checks) == 0 && (pullRequest.CheckRunCount > 0 || pullRequest.StatusContextCount > 0) {
		return false, false
	}
	return false, true
}

func evaluateBudgetCapacityPredicate(
	ctx context.Context,
	o *Orchestrator,
	predicateContext *blockerPredicateContext,
	predicate workpad.Predicate,
) (string, string) {
	switch predicate.Scope {
	case "daily", "daily_budget":
		if o.dailyBudgetStatus == nil {
			return blockerEvidenceStatusUnverifiable, "daily budget status provider is unavailable"
		}
		status, known, err := o.dailyBudgetStatus.DailyBudgetStatus(ctx, predicateContext.now)
		if err != nil || !known {
			return blockerEvidenceStatusUnverifiable, "daily budget status could not be refreshed"
		}
		return capacityConditionStatus(status.Active && status.MaxUSD > 0 && status.CurrentSpendUSD >= status.MaxUSD, predicate.Condition, "daily budget")
	case "issue", "issue_budget":
		if o.issueBudgetStatus == nil {
			return blockerEvidenceStatusUnverifiable, "issue budget status provider is unavailable"
		}
		status, known, err := o.issueBudgetStatus.IssueBudgetStatus(ctx, predicateContext.issue)
		if err != nil || !known {
			return blockerEvidenceStatusUnverifiable, "issue budget status could not be refreshed"
		}
		return capacityConditionStatus(status.Active && status.MaxUSD > 0 && status.CurrentSpendUSD >= status.MaxUSD, predicate.Condition, "issue budget")
	case "global", "global_capacity":
		if predicateContext.state == nil || predicateContext.state.MaxConcurrentAgents <= 0 {
			return blockerEvidenceStatusUnverifiable, "global capacity limit is unavailable"
		}
		exhausted := activeAgentClaims(predicateContext.state) >= predicateContext.state.MaxConcurrentAgents
		return capacityConditionStatus(exhausted, predicate.Condition, "global capacity")
	case "backend", "backend_capacity":
		if predicateContext.state == nil {
			return blockerEvidenceStatusUnverifiable, "backend capacity state is unavailable"
		}
		exhausted := backendCapacityOutageMatches(predicateContext.state.BackendOutages, predicate.Resource)
		return capacityConditionStatus(exhausted, predicate.Condition, "backend capacity")
	default:
		return blockerEvidenceStatusUnverifiable, "budget or capacity scope is not registered"
	}
}

func capacityConditionStatus(exhausted bool, condition string, label string) (string, string) {
	condition = strings.ToLower(strings.TrimSpace(condition))
	holds := exhausted
	if condition == "available" {
		holds = !exhausted
	}
	if holds {
		return blockerEvidenceStatusHolds, label + " condition still holds"
	}
	return blockerEvidenceStatusCleared, label + " condition no longer holds"
}

func activeAgentClaims(state *State) int {
	if state == nil {
		return 0
	}
	active := map[string]struct{}{}
	for issueID := range state.Running {
		active[issueID] = struct{}{}
	}
	for issueID := range state.Claimed {
		active[issueID] = struct{}{}
	}
	return len(active)
}

func backendCapacityOutageMatches(outages map[string]BackendOutage, resource string) bool {
	resource = strings.ToLower(strings.TrimSpace(resource))
	for _, outage := range outages {
		scope := (backendcapacity.Scope{
			BackendID:   outage.Scope.BackendID,
			BackendKind: outage.Scope.BackendKind,
			Provider:    outage.Scope.Provider,
		}).Normalize()
		if resource == "" || strings.EqualFold(scope.BackendID, resource) || strings.EqualFold(scope.BackendKind, resource) || strings.EqualFold(scope.Provider, resource) {
			return true
		}
	}
	return false
}

func evaluateConfigFingerprintPredicate(
	ctx context.Context,
	o *Orchestrator,
	predicateContext *blockerPredicateContext,
	predicate workpad.Predicate,
) (string, string) {
	signals := o.blockedCauseSignals(ctx, predicateContext.issue, RunModeImplement, predicateContext.issue.State, DiffStats{})
	if strings.TrimSpace(signals.ConfigFingerprint) == "" {
		return blockerEvidenceStatusUnverifiable, "live config fingerprint is unavailable"
	}
	if strings.TrimSpace(signals.ConfigFingerprint) == strings.TrimSpace(predicate.Fingerprint) {
		return blockerEvidenceStatusHolds, "config fingerprint is unchanged"
	}
	return blockerEvidenceStatusCleared, "config fingerprint changed"
}

func tokenInList(value string, values []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range values {
		if value == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func setBlockedEvidence(state *State, issueID string, evidence []telemetry.BlockerEvidence) {
	if state == nil {
		return
	}
	issueID = strings.TrimSpace(issueID)
	entry, ok := state.Blocked[issueID]
	if !ok {
		return
	}
	entry.BlockerEvidence = cloneBlockerEvidence(evidence)
	state.Blocked[issueID] = entry
}

func (o *Orchestrator) applyRecordedBlockerRecovery(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	dependencies []dependencyBlocker,
	evidence []telemetry.BlockerEvidence,
	now time.Time,
) bool {
	targetState := dependencyAutoUnblockTargetState(state, issue, normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock).TargetState)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("recorded blocker evidence encoding failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return false
	}
	signature := workpad.ContentHash(strings.TrimSpace(issue.Identifier) + "\n" + string(encoded))
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionRecordedBlockerRecovery, signature)
	if err := o.updateIssueStateByIDStrictWithMetadata(
		ctx,
		state,
		strings.TrimSpace(issue.ID),
		issue,
		targetState,
		now,
		workflowActionRecordedBlockerRecovery,
		metadata,
		laneMutationPreserveOwnership,
	); err != nil {
		if o.logger != nil {
			o.logger.Warn("recorded blocker recovery failed", "issue_id", issue.ID, "identifier", issue.Identifier, "target_state", targetState, "error", err)
		}
		return false
	}
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issue.ID, recordedBlockerRecoveryComment(issue, targetState, dependencies, evidence)); err != nil && o.logger != nil {
			o.logger.Warn("recorded blocker recovery comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Blocked, strings.TrimSpace(issue.ID))
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   workflowActionRecordedBlockerRecovery,
		Message: "cleared recorded blockers for " + issueLabel(issue) + " and moved it to " + targetState,
	})
	return true
}

func recordedBlockerRecoveryComment(
	issue connector.Issue,
	targetState string,
	dependencies []dependencyBlocker,
	evidence []telemetry.BlockerEvidence,
) string {
	var b strings.Builder
	b.WriteString("Recorded blocker predicates cleared. Moved ")
	b.WriteString(issueLabel(issue))
	b.WriteString(" from ")
	b.WriteString(strings.TrimSpace(issue.State))
	b.WriteString(" to ")
	b.WriteString(strings.TrimSpace(targetState))
	b.WriteString(".")
	b.WriteString("\n\nCleared evidence:")
	for _, item := range evidence {
		b.WriteString("\n- ")
		b.WriteString(strings.TrimSpace(item.Type))
		if item.Reference != "" {
			b.WriteString(" ")
			b.WriteString(strings.TrimSpace(item.Reference))
		}
		b.WriteString(" (owner: ")
		b.WriteString(strings.TrimSpace(item.Owner))
		b.WriteString(")")
	}
	for _, dependency := range dependencies {
		b.WriteString("\n- issue_state ")
		b.WriteString(dependencyBlockerLabel(dependency))
		b.WriteString(" (owner: orchestrator)")
	}
	return b.String()
}
