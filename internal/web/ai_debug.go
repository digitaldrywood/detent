package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/aidebug"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) aiDebugPrompt(c echo.Context) error {
	scope := aidebug.Scope(strings.TrimSpace(c.QueryParam("scope")))
	if scope == "" {
		scope = aidebug.ScopeIssue
	}
	projection, err := s.aiDebugProjection(c.Request().Context(), scope, c.QueryParam("project"), c.QueryParam("issue"))
	if err != nil {
		if errors.Is(err, errAIDebugNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "AI Debug target not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "AI Debug prompt could not be assembled").SetInternal(err)
	}
	prompt, err := projection.Prompt()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "AI Debug prompt could not be rendered").SetInternal(err)
	}
	return c.Blob(http.StatusOK, echo.MIMETextPlainCharsetUTF8, []byte(prompt))
}

var errAIDebugNotFound = errors.New("AI Debug target not found")

func (s *Server) aiDebugProjection(ctx context.Context, scope aidebug.Scope, projectID string, issueRef string) (aidebug.Projection, error) {
	now := s.now().UTC()
	snapshot := s.cachedEnrichedSnapshot(ctx, s.latestSnapshot(ctx))
	projection := aidebug.NewProjection(scope, now)
	host, err := s.hostname()
	if err != nil {
		projection.EvidenceGaps = append(projection.EvidenceGaps, "host lookup failed: "+err.Error())
	}
	projection.Detent = aidebug.DetentEvidence{
		Version:      strings.TrimSpace(s.version),
		Host:         strings.TrimSpace(host),
		InstanceName: s.instanceName(),
	}
	projection.Fleet = aiDebugFleetEvidence(snapshot)

	switch scope {
	case aidebug.ScopeFleet:
		return projection, nil
	case aidebug.ScopeProject, aidebug.ScopeIssue:
	default:
		return aidebug.Projection{}, fmt.Errorf("unsupported AI Debug scope %q", scope)
	}

	projectID = strings.TrimSpace(projectID)
	trackedProject, ok := s.registry.Get(project.ID(projectID))
	if !ok {
		return aidebug.Projection{}, errAIDebugNotFound
	}
	projectEvidence, projectGaps := aiDebugProjectEvidence(trackedProject, snapshot)
	projection.Project = &projectEvidence
	projection.EvidenceGaps = append(projection.EvidenceGaps, projectGaps...)
	if scope == aidebug.ScopeProject {
		return projection, nil
	}

	issue, ok := aiDebugFindIssue(snapshot, projectID, issueRef)
	if !ok {
		return aidebug.Projection{}, errAIDebugNotFound
	}
	issueEvidence, gaps, err := s.aiDebugIssueEvidence(ctx, trackedProject, snapshot, issue)
	if err != nil {
		return aidebug.Projection{}, err
	}
	projection.Issue = &issueEvidence
	projection.EvidenceGaps = append(projection.EvidenceGaps, gaps...)
	return projection, nil
}

func aiDebugFleetEvidence(snapshot telemetry.Snapshot) aidebug.FleetEvidence {
	github := map[string]any{}
	provider := map[string]any{}
	if snapshot.RateLimits != nil {
		provider = map[string]any{
			"limit_id":     snapshot.RateLimits.LimitID,
			"limit_name":   snapshot.RateLimits.LimitName,
			"reached_type": snapshot.RateLimits.ReachedType,
			"primary":      snapshot.RateLimits.Primary,
			"secondary":    snapshot.RateLimits.Secondary,
			"credits":      snapshot.RateLimits.Credits,
		}
		github = map[string]any{
			"graphql":                       snapshot.RateLimits.GitHubGraphQL,
			"rest":                          snapshot.RateLimits.GitHubREST,
			"rest_endpoint_budgets":         snapshot.RateLimits.GitHubRESTBudgets,
			"graphql_cost_by_query_family":  snapshot.RateLimits.GraphQLCost,
			"rest_usage_by_endpoint_family": snapshot.RateLimits.RESTUsage,
		}
	}
	return aidebug.FleetEvidence{
		AgentPools:        aiDebugJSON(snapshot.AgentPools),
		RunningCount:      len(snapshot.Running),
		ProviderRateState: aiDebugJSON(provider),
		GitHubBudgets:     aiDebugJSON(github),
		Dispatch:          aiDebugJSON(map[string]any{"fleet": snapshot.Dispatch, "project_stalls": snapshot.DispatchStalls}),
		CapacityConditions: aiDebugJSON(map[string]any{
			"backend_outages":     snapshot.BackendOutages,
			"failure_breakers":    snapshot.FailureBreakers,
			"memory_pressure":     snapshot.MemoryPressure,
			"dispatch_recoveries": snapshot.DispatchRecoveries,
		}),
	}
}

func aiDebugProjectEvidence(trackedProject *project.Project, snapshot telemetry.Snapshot) (aidebug.ProjectEvidence, []string) {
	projectConfig := trackedProject.Config()
	workflow := trackedProject.Workflow().Config
	source := trackedProject.WorkflowSourceStatus()
	drift := "not detected by runtime"
	gaps := []string{}
	if source.LastReloadError != "" {
		drift = "runtime reload error"
	} else if strings.TrimSpace(projectConfig.WorkflowRef) != "" {
		drift = "unknown: runtime does not expose committed-ref comparison"
		gaps = append(gaps, "workflow drift from committed ref could not be proven or disproven from runtime evidence")
	}
	dispatch := snapshot.Dispatch
	for _, candidate := range snapshot.DispatchStalls {
		if strings.TrimSpace(candidate.ProjectID) == string(trackedProject.ID()) {
			dispatch = candidate
			break
		}
	}
	projectAuthorization := aiDebugMap(projectConfig.Authorization)
	trackerAuthorization := aiDebugMap(workflow.Tracker.Authorization)
	authorization := map[string]any{
		"project_selector":             projectAuthorization,
		"tracker_selector":             trackerAuthorization,
		"project_selector_description": selector.Describe(projectConfig.Authorization, selector.Context{}),
		"tracker_selector_description": selector.Describe(workflow.Tracker.Authorization, selector.Context{}),
		"combination":                  "both configured selectors must allow the issue",
	}
	modifiedAt := aiDebugTimePointer(source.ModifiedAt)
	loadedAt := aiDebugTimePointer(source.LoadedAt)
	reconciledAt := aiDebugTimePointer(source.LastReconcileAt)
	return aidebug.ProjectEvidence{
		ID:                          string(trackedProject.ID()),
		Repository:                  strings.TrimSpace(workflow.Tracker.Repository),
		TrackerKind:                 strings.TrimSpace(workflow.Tracker.Kind),
		DetentDefectDestinationRepo: "digitaldrywood/detent",
		ConfigDestinationRepo:       strings.TrimSpace(workflow.Tracker.Repository),
		Brakes: aidebug.BrakeEvidence{
			NoProgressLimit:      workflow.Agent.AutoPromote.NoProgressLimit,
			MaxSessionTokens:     workflow.Agent.MaxSessionTokens,
			LifetimeSessionLimit: workflow.Agent.LifetimeSessionLimit,
			LifetimeTokenLimit:   workflow.Agent.LifetimeTokenLimit,
			MaxConcurrentAgents:  workflow.Agent.MaxConcurrentAgents,
			RateWindowPacing:     workflow.Agent.RateWindowPacing,
			BillingMode:          workflow.Budget.EffectiveBillingMode(),
		},
		Authorization: authorization,
		Workflow: aidebug.WorkflowEvidence{
			ConfiguredPath:  strings.TrimSpace(projectConfig.Workflow),
			CommittedRef:    strings.TrimSpace(projectConfig.WorkflowRef),
			LoadedPath:      strings.TrimSpace(source.Path),
			LoadedHash:      strings.TrimSpace(source.Hash),
			Revision:        strings.TrimSpace(source.Revision),
			ModifiedAt:      modifiedAt,
			LoadedAt:        loadedAt,
			LastReconcileAt: reconciledAt,
			DriftStatus:     drift,
			LastReloadError: strings.TrimSpace(source.LastReloadError),
		},
		GateDefinition: aiDebugJSON(workflow.Gate),
		LastGateResult: aiDebugProjectGateResult(snapshot, string(trackedProject.ID())),
		Dispatch:       aiDebugJSON(dispatch),
	}, gaps
}

func (s *Server) aiDebugIssueEvidence(ctx context.Context, trackedProject *project.Project, snapshot telemetry.Snapshot, issue telemetry.Issue) (aidebug.IssueEvidence, []string, error) {
	identity := store.IssueIdentity{
		ProjectID:  string(trackedProject.ID()),
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
	}
	gaps := []string{}
	var err error
	attempts := []store.WorkAttempt{}
	if reader, ok := s.store.(store.AIDebugAttemptReader); ok {
		attempts, err = reader.ListIssueAIDebugWorkAttempts(ctx, identity)
		if err != nil {
			return aidebug.IssueEvidence{}, nil, fmt.Errorf("AI Debug work attempts: %w", err)
		}
	} else {
		gaps = append(gaps, "work-attempt history reader is unavailable")
	}
	sessions := []store.AIDebugSession{}
	if reader, ok := s.store.(store.AIDebugSessionReader); ok {
		sessions, err = reader.ListIssueAIDebugSessions(ctx, identity)
		if err != nil {
			return aidebug.IssueEvidence{}, nil, fmt.Errorf("AI Debug sessions: %w", err)
		}
	} else {
		gaps = append(gaps, "session history reader is unavailable")
	}
	decisions := []store.SchedulerDecision{}
	if reader, ok := s.store.(store.IssueSchedulerDecisionStore); ok {
		decisions, err = reader.ListIssueSchedulerDecisions(ctx, store.IssueSchedulerDecisionQuery{Identity: identity, Limit: 100})
		if err != nil {
			return aidebug.IssueEvidence{}, nil, fmt.Errorf("AI Debug scheduler decisions: %w", err)
		}
	} else {
		gaps = append(gaps, "issue scheduler-decision reader is unavailable")
	}
	timeline, err := s.store.IssueWorkflowTimeline(ctx, identity)
	if err != nil {
		return aidebug.IssueEvidence{}, nil, fmt.Errorf("AI Debug lane timeline: %w", err)
	}

	blocked, blockedFound := aiDebugBlocked(snapshot, issue)
	evidence := aidebug.IssueEvidence{
		ID:                 strings.TrimSpace(issue.ID),
		Identifier:         strings.TrimSpace(issue.Identifier),
		Title:              strings.TrimSpace(issue.Title),
		URL:                strings.TrimSpace(issue.URL),
		ProjectID:          identity.ProjectID,
		TrackerKind:        strings.TrimSpace(trackedProject.Workflow().Config.Tracker.Kind),
		TrackerState:       strings.TrimSpace(issue.State),
		RuntimeState:       aiDebugRuntimeState(snapshot, issue),
		CurrentLane:        strings.TrimSpace(issue.State),
		TimeInLaneSeconds:  issue.CurrentLaneAgeSeconds,
		Blocked:            aiDebugBlockedEvidence(issue, blocked, blockedFound),
		Park:               aiDebugParkEvidence(snapshot, issue, trackedProject.Workflow().Config.Agent.AutoPromote.NoProgressLimit),
		Dependencies:       aiDebugDependencies(snapshot, issue),
		Attempts:           aiDebugAttempts(attempts),
		Sessions:           aiDebugSessions(sessions),
		SchedulerDecisions: aiDebugSchedulerDecisions(decisions),
		LaneTransitions:    aiDebugLaneTransitions(timeline),
		Delivery:           aiDebugDelivery(issue, attempts),
		HookAndCIErrors:    aiDebugHookAndCIErrors(issue, attempts),
	}
	evidence.StateDisagreement = !strings.EqualFold(strings.TrimSpace(evidence.TrackerState), strings.TrimSpace(evidence.RuntimeState))
	aidebug.FinalizeAggregates(&evidence)
	return evidence, gaps, nil
}

func aiDebugFindIssue(snapshot telemetry.Snapshot, projectID string, issueRef string) (telemetry.Issue, bool) {
	issueRef = strings.TrimSpace(issueRef)
	for _, issue := range aiDebugAllIssues(snapshot) {
		if strings.TrimSpace(issue.ProjectID) != "" && strings.TrimSpace(issue.ProjectID) != projectID {
			continue
		}
		if issueRef == strings.TrimSpace(issue.ID) || issueRef == strings.TrimSpace(issue.Identifier) || issueRef == strings.TrimSpace(issue.URL) {
			return issue, true
		}
	}
	return telemetry.Issue{}, false
}

func aiDebugAllIssues(snapshot telemetry.Snapshot) []telemetry.Issue {
	issues := append([]telemetry.Issue{}, snapshot.BoardIssues...)
	issues = append(issues, snapshot.Pipeline...)
	for _, row := range snapshot.Running {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Queue {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Blocked {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Completed {
		issues = append(issues, row.Issue)
	}
	return issues
}

func aiDebugRuntimeState(snapshot telemetry.Snapshot, issue telemetry.Issue) string {
	for _, row := range snapshot.Running {
		if aiDebugIssueMatches(row.Issue, issue) {
			return "running"
		}
	}
	for _, row := range snapshot.Blocked {
		if aiDebugIssueMatches(row.Issue, issue) {
			return "blocked"
		}
	}
	for _, row := range snapshot.Queue {
		if aiDebugIssueMatches(row.Issue, issue) {
			return "queued"
		}
	}
	for _, row := range snapshot.Completed {
		if aiDebugIssueMatches(row.Issue, issue) {
			return "completed"
		}
	}
	return strings.TrimSpace(issue.State)
}

func aiDebugBlocked(snapshot telemetry.Snapshot, issue telemetry.Issue) (telemetry.Blocked, bool) {
	for _, row := range snapshot.Blocked {
		if aiDebugIssueMatches(row.Issue, issue) {
			return row, true
		}
	}
	return telemetry.Blocked{}, false
}

func aiDebugBlockedEvidence(issue telemetry.Issue, blocked telemetry.Blocked, found bool) aidebug.BlockedEvidence {
	evidence := aidebug.BlockedEvidence{Cause: "No blocked cause is recorded.", CauseAuthor: "none", RecoveryPredicate: []map[string]any{}}
	if found {
		evidence.Source = string(blocked.Source)
		evidence.RecoveryAction = strings.TrimSpace(blocked.RecoveryAction)
		evidence.RecoveryReason = strings.TrimSpace(blocked.RecoveryReason)
		evidence.RecoveryTarget = strings.TrimSpace(blocked.RecoveryTarget)
		evidence.Remedy = strings.TrimSpace(blocked.RecoveryRemedy)
		for _, predicate := range blocked.BlockerEvidence {
			evidence.RecoveryPredicate = append(evidence.RecoveryPredicate, aiDebugMap(predicate))
		}
		cause := strings.TrimSpace(blocked.Error)
		if cause == "" {
			cause = strings.TrimSpace(blocked.RecoveryReason)
		}
		if cause == "" {
			cause = strings.TrimSpace(string(blocked.Source))
		}
		if cause != "" {
			evidence.CausePresent = true
			evidence.Cause = cause
			evidence.CauseAuthor = "detent"
		}
	}
	if !evidence.CausePresent && len(issue.BlockedBy) > 0 {
		evidence.CausePresent = true
		evidence.Cause = "tracker dependency references are unresolved"
		evidence.CauseAuthor = "tracker"
	}
	return evidence
}

func aiDebugParkEvidence(snapshot telemetry.Snapshot, issue telemetry.Issue, noProgressLimit int) aidebug.ParkEvidence {
	evidence := aidebug.ParkEvidence{
		Thresholds:        map[string]int64{"no_progress_limit": int64(noProgressLimit)},
		ConsecutiveCounts: map[string]int64{"no_progress": int64(issue.CompletionProgress.ConsecutiveNoProgress)},
		AttemptCount:      issue.ParkSummary.AttemptCount,
		ParkCount:         issue.ParkSummary.ParkCount,
		Causes:            []map[string]any{},
	}
	for _, cause := range issue.ParkSummary.Causes {
		evidence.Causes = append(evidence.Causes, aiDebugMap(cause))
	}
	evidence.Parked = evidence.ParkCount > 0
	for _, loop := range snapshot.DispatchLoops {
		if loop.ProjectID == issue.ProjectID && (loop.IssueID == issue.ID || loop.Identifier == issue.Identifier) {
			evidence.BreakerKind = "dispatch_loop"
			evidence.Thresholds["dispatch_limit"] = int64(loop.DispatchLimit)
			evidence.ConsecutiveCounts["dispatches"] = int64(loop.ConsecutiveDispatches)
			evidence.Parked = evidence.Parked || loop.Tripped
		}
	}
	for _, breaker := range snapshot.FailureBreakers {
		for _, item := range breaker.Items {
			if item.IssueID != issue.ID && item.Identifier != issue.Identifier {
				continue
			}
			evidence.BreakerKind = "failure_breaker"
			evidence.Thresholds["window_seconds"] = breaker.WindowSeconds
			evidence.Thresholds["cooldown_seconds"] = breaker.CooldownSeconds
			evidence.ConsecutiveCounts["same_class_failures"] = int64(item.AttemptCount)
			evidence.Parked = evidence.Parked || item.Parked
			if !breaker.ResumeAt.IsZero() {
				value := breaker.ResumeAt.UTC()
				evidence.CooldownExpiresAt = &value
			}
		}
	}
	return evidence
}

func aiDebugDependencies(snapshot telemetry.Snapshot, issue telemetry.Issue) []aidebug.DependencyEvidence {
	byRef := map[string]telemetry.Issue{}
	for _, candidate := range aiDebugAllIssues(snapshot) {
		for _, ref := range []string{candidate.ID, candidate.Identifier, candidate.URL} {
			if ref = strings.TrimSpace(ref); ref != "" {
				byRef[ref] = candidate
			}
		}
	}
	result := []aidebug.DependencyEvidence{}
	seen := map[string]bool{}
	var visit func([]telemetry.BlockedRef, int)
	visit = func(refs []telemetry.BlockedRef, depth int) {
		for _, ref := range refs {
			key := strings.TrimSpace(ref.Identifier)
			if key == "" {
				key = strings.TrimSpace(ref.ID)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			state := strings.TrimSpace(ref.State)
			if state == "" {
				state = strings.TrimSpace(ref.TrackerState)
			}
			result = append(result, aidebug.DependencyEvidence{Depth: depth, ID: ref.ID, Identifier: ref.Identifier, CurrentState: state, TrackerState: ref.TrackerState, Source: ref.Source})
			if next, ok := byRef[key]; ok {
				visit(next.BlockedBy, depth+1)
			}
		}
	}
	visit(issue.BlockedBy, 1)
	return result
}

func aiDebugAttempts(attempts []store.WorkAttempt) []aidebug.AttemptEvidence {
	result := make([]aidebug.AttemptEvidence, 0, len(attempts))
	for _, attempt := range attempts {
		metadata := aiDebugDecodeObject(attempt.WorkerMetadataJSON)
		metrics := aiDebugDecodeObject(attempt.MetricsJSON)
		completedAt := aiDebugTimePointer(attempt.CompletedAt)
		result = append(result, aidebug.AttemptEvidence{
			ID: attempt.ID, StartedAt: attempt.StartedAt.UTC(), CompletedAt: completedAt,
			TerminalState: string(attempt.TerminalState), AttemptNumber: attempt.AttemptNumber, Lane: attempt.Lane,
			ErrorClass: attempt.ErrorClass, ErrorMessage: attempt.ErrorMessage, PRNumber: attempt.PRNumber,
			WorkspaceDiffstat:   aiDebugStringValue(metadata, metrics, "workspace_diffstat", "diffstat"),
			UnpushedCommitCount: aiDebugIntPointer(metadata, metrics, "unpushed_commit_count"),
			WorkProductPushed:   aiDebugBoolPointer(metadata, metrics, "work_product_pushed"),
			CIState:             attempt.CIState, WorkerMetadataJSON: attempt.WorkerMetadataJSON, MetricsJSON: attempt.MetricsJSON,
			CapacitySnapshotJSON: attempt.CapacitySnapshotJSON, GitHubRateSnapshotJSON: attempt.GitHubRateSnapshotJSON,
		})
	}
	return result
}

func aiDebugSessions(sessions []store.AIDebugSession) []aidebug.SessionEvidence {
	result := make([]aidebug.SessionEvidence, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, aidebug.SessionEvidence{
			ID: session.ID, WorkAttemptID: session.WorkAttemptID, StartedAt: session.StartedAt, CompletedAt: session.CompletedAt,
			InputTokens: session.InputTokens, CachedInputTokens: session.CachedInputTokens, OutputTokens: session.OutputTokens,
			ReasoningOutputTokens: session.ReasoningOutputTokens, TotalTokens: session.TotalTokens, Turns: session.Turns,
			Model: session.Model, RequestedModel: session.RequestedModel, Effort: session.Effort, RuntimeSeconds: session.RuntimeSeconds,
			FinalState: session.FinalState, ResumedFromSessionID: session.ResumedFromSessionID,
		})
	}
	return result
}

func aiDebugSchedulerDecisions(decisions []store.SchedulerDecision) []aidebug.SchedulerDecisionEvidence {
	result := make([]aidebug.SchedulerDecisionEvidence, 0, len(decisions))
	for _, decision := range decisions {
		result = append(result, aidebug.SchedulerDecisionEvidence{At: decision.DecisionAt.UTC(), Result: string(decision.Result), Reason: decision.Reason, WaitReason: decision.WaitReason, AttemptNumber: decision.AttemptNumber, QueuePosition: decision.QueuePosition, CapacityJSON: decision.CapacitySnapshotJSON, GitHubRateJSON: decision.GitHubRateSnapshotJSON})
	}
	return result
}

func aiDebugLaneTransitions(timeline store.WorkflowTimeline) []aidebug.LaneTransitionEvidence {
	result := []aidebug.LaneTransitionEvidence{}
	for _, event := range timeline.Events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || !strings.EqualFold(event.Status, "entered") {
			continue
		}
		origin := "indeterminate"
		if metadata, ok := provenance.Parse(event.MetadataJSON); ok {
			origin = string(metadata.Provenance.Origin)
		}
		result = append(result, aidebug.LaneTransitionEvidence{At: event.StartedAt.UTC(), From: event.PreviousPhaseName, To: event.PhaseName, Origin: origin, MutationReason: event.Reason})
	}
	return result
}

func aiDebugDelivery(issue telemetry.Issue, attempts []store.WorkAttempt) aidebug.DeliveryEvidence {
	evidence := aidebug.DeliveryEvidence{HeadMovedAcrossAttempts: "unknown: no PR head SHA was recorded in attempt metadata", WorkProductPushed: "unknown"}
	headSHAs := map[string]bool{}
	for _, attempt := range attempts {
		metadata := aiDebugDecodeObject(attempt.WorkerMetadataJSON)
		metrics := aiDebugDecodeObject(attempt.MetricsJSON)
		if value := aiDebugStringValue(metadata, metrics, "current_head_sha", "pr_head_sha", "head_sha"); value != "" {
			headSHAs[value] = true
		}
		if value := aiDebugBoolPointer(metadata, metrics, "work_product_pushed"); value != nil {
			evidence.WorkProductPushed = strconv.FormatBool(*value)
		}
	}
	if len(headSHAs) == 1 {
		evidence.HeadMovedAcrossAttempts = "false"
	} else if len(headSHAs) > 1 {
		evidence.HeadMovedAcrossAttempts = "true"
	}
	if issue.PullRequest == nil {
		return evidence
	}
	pr := issue.PullRequest
	evidence.PRNumber = pr.Number
	evidence.State = pr.State
	evidence.MergeableStatus = pr.MergeableState
	evidence.HeadSHA = pr.HeadSHA
	evidence.BranchName = pr.BranchName
	for _, check := range aiDebugPRChecks(pr) {
		evidence.Checks = append(evidence.Checks, aiDebugMap(check))
	}
	return evidence
}

func aiDebugHookAndCIErrors(issue telemetry.Issue, attempts []store.WorkAttempt) []string {
	values := []string{}
	for _, attempt := range attempts {
		if attempt.ErrorMessage != "" {
			values = append(values, attempt.ErrorMessage)
		}
		if attempt.CIState != "" && !strings.EqualFold(attempt.CIState, "success") {
			values = append(values, "CI state: "+attempt.CIState)
		}
	}
	if issue.PullRequest != nil {
		for _, check := range aiDebugPRChecks(issue.PullRequest) {
			if strings.EqualFold(check.Conclusion, "success") || strings.EqualFold(check.Status, "completed") && check.Conclusion == "" {
				continue
			}
			values = append(values, strings.TrimSpace(check.Name+": "+check.Status+" "+check.Conclusion))
		}
	}
	return aiDebugUniqueTrimmed(values)
}

func aiDebugPRChecks(pr *telemetry.PullRequest) []telemetry.PullRequestCheck {
	if pr == nil {
		return nil
	}
	checks := append([]telemetry.PullRequestCheck{}, pr.SlowChecks...)
	checks = append(checks, pr.UnstartedChecks...)
	checks = append(checks, pr.RequiredCheckFailures...)
	for _, name := range pr.RunningChecks {
		checks = append(checks, telemetry.PullRequestCheck{Name: name, Status: "running"})
	}
	return checks
}

func aiDebugUniqueTrimmed(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func aiDebugProjectGateResult(snapshot telemetry.Snapshot, projectID string) string {
	for _, attempt := range snapshot.WorkAttempts {
		if strings.TrimSpace(attempt.ProjectID) == strings.TrimSpace(projectID) && strings.TrimSpace(attempt.CIState) != "" {
			return attempt.CIState
		}
	}
	return "No gate result is recorded in the current snapshot."
}

func aiDebugIssueMatches(left telemetry.Issue, right telemetry.Issue) bool {
	return strings.TrimSpace(left.ProjectID) == strings.TrimSpace(right.ProjectID) && (strings.TrimSpace(left.ID) != "" && left.ID == right.ID ||
		strings.TrimSpace(left.Identifier) != "" && left.Identifier == right.Identifier ||
		strings.TrimSpace(left.URL) != "" && left.URL == right.URL)
}

func aiDebugJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return data
}

func aiDebugMap(value any) map[string]any {
	result := map[string]any{}
	if err := json.Unmarshal(aiDebugJSON(value), &result); err != nil {
		return map[string]any{}
	}
	return result
}

func aiDebugDecodeObject(raw string) map[string]any {
	value := map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &value); err != nil {
		return map[string]any{}
	}
	return value
}

func aiDebugNestedValue(objects []map[string]any, keys ...string) any {
	var search func(map[string]any) any
	search = func(object map[string]any) any {
		for key, value := range object {
			for _, candidate := range keys {
				if strings.EqualFold(key, candidate) {
					return value
				}
			}
			if nested, ok := value.(map[string]any); ok {
				if found := search(nested); found != nil {
					return found
				}
			}
		}
		return nil
	}
	for _, object := range objects {
		if value := search(object); value != nil {
			return value
		}
	}
	return nil
}

func aiDebugStringValue(first map[string]any, second map[string]any, keys ...string) string {
	value := aiDebugNestedValue([]map[string]any{first, second}, keys...)
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func aiDebugIntPointer(first map[string]any, second map[string]any, keys ...string) *int64 {
	value := aiDebugNestedValue([]map[string]any{first, second}, keys...)
	if number, ok := value.(float64); ok {
		result := int64(number)
		return &result
	}
	return nil
}

func aiDebugBoolPointer(first map[string]any, second map[string]any, keys ...string) *bool {
	value := aiDebugNestedValue([]map[string]any{first, second}, keys...)
	if flag, ok := value.(bool); ok {
		return &flag
	}
	return nil
}

func aiDebugTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
