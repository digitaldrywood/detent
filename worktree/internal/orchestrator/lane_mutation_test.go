package orchestrator

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func laneMutationTestConfig() Config {
	return normalizeConfig(Config{
		Project:        scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review", "Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			PassState:   "Merging",
			ReworkState: "Rework",
		},
	})
}

func openLaneMutationTestStore(
	t *testing.T,
	ctx context.Context,
	projectID string,
	issue connector.Issue,
	now time.Time,
) (*runtimeFixture, int64) {
	t.Helper()

	runtimeStore := newRuntimeFixture()
	attemptID, err := runtimeStore.StartWorkAttempt(ctx, store.WorkAttemptStart{
		ProjectID:      projectID,
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		WorkerType:     "agent",
		Lane:           issue.State,
		AttemptNumber:  1,
		StartedAt:      now.Add(-time.Minute),
		LeaseExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	return runtimeStore, attemptID
}

type runtimeFixture struct {
	autoPromoteWorkflowMetricsRecorder
	validatorMemoFixture
	*securityAuditMemoryStore
	attempts runtimeFixtureAttempts
	ledger   runtimeFixtureLedger
	resume   store.AgentResumeState
}

func newRuntimeFixture() *runtimeFixture {
	return &runtimeFixture{securityAuditMemoryStore: newSecurityAuditMemoryStore()}
}

type RuntimeFixtureForTest = runtimeFixture

func NewRuntimeFixtureForTest() *RuntimeFixtureForTest {
	return newRuntimeFixture()
}

type runtimeFixtureAttempts struct {
	mu        sync.Mutex
	attempts  []store.WorkAttempt
	decisions []store.SchedulerDecision
}

type runtimeFixtureLaneWrite struct {
	project string
	write   coordination.LaneWrite
	result  string
	origin  string
	kind    string
}

type runtimeFixtureLedger struct {
	lock         sync.Mutex
	mu           sync.Mutex
	writes       []runtimeFixtureLaneWrite
	observations map[store.IssueIdentity]store.LaneObservation
}

func runtimeFixtureTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC().Truncate(time.Second)
}

func runtimeFixtureJSON(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return "{}"
}

func runtimeFixtureMetadataFields(raw string) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return nil
	}
	return fields
}

func runtimeFixtureMetadataString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func runtimeFixtureOutcomeExcluded(attempt store.WorkAttempt) bool {
	fence := runtimeFixtureMetadataFields(string(runtimeFixtureMetadataFields(attempt.WorkerMetadataJSON)["historical_completion_fence"]))
	raw := fence["excluded_from_worker_outcomes"]
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return false
	}
	var number float64
	return raw[0] == '"' || json.Unmarshal(raw, &number) != nil || number != 0
}

func runtimeFixtureForgeWait(attempt store.WorkAttempt) (present bool, host, errorClass, completedHost string) {
	fields := runtimeFixtureMetadataFields(attempt.WorkerMetadataJSON)
	wait := runtimeFixtureMetadataFields(string(fields["forge_wait"]))
	return wait != nil, runtimeFixtureMetadataString(wait["host"]), runtimeFixtureMetadataString(wait["error_class"]), runtimeFixtureMetadataString(fields["forge_write_completed_host"])
}

func runtimeFixtureTerminal(attempt store.WorkAttempt) bool {
	return !attempt.CompletedAt.IsZero() && attempt.Status == store.WorkAttemptStatusTerminal
}

func runtimeFixtureCompletedBefore(left, right store.WorkAttempt) int {
	if order := left.CompletedAt.Compare(right.CompletedAt); order != 0 {
		return order
	}
	return cmp.Compare(left.ID, right.ID)
}

func (f *runtimeFixture) find(id int64) *store.WorkAttempt {
	for index := range f.attempts.attempts {
		if f.attempts.attempts[index].ID == id {
			return &f.attempts.attempts[index]
		}
	}
	return nil
}

func (f *runtimeFixture) StartWorkAttempt(ctx context.Context, start store.WorkAttemptStart) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(start.ProjectID) == "" {
		return 0, errors.New("project_id is required")
	}
	if strings.TrimSpace(start.WorkerType) == "" {
		return 0, errors.New("worker_type is required")
	}
	if start.StartedAt.IsZero() {
		return 0, errors.New("started_at is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	number := start.AttemptNumber
	if number <= 0 {
		number = 1
	}
	startedAt := runtimeFixtureTime(start.StartedAt)
	attempt := store.WorkAttempt{
		ID: int64(len(f.attempts.attempts) + 1), ProjectID: strings.TrimSpace(start.ProjectID), IssueID: start.IssueID,
		Identifier: start.Identifier, IssueURL: start.IssueURL, PRNumber: start.PRNumber, Repo: start.Repo,
		WorkerType: strings.TrimSpace(start.WorkerType), WorkerHost: start.WorkerHost, Lane: start.Lane, AttemptNumber: number,
		Status: store.WorkAttemptStatusActive, StartedAt: startedAt, LeaseExpiresAt: runtimeFixtureTime(start.LeaseExpiresAt),
		HeartbeatAt: startedAt, Phase: start.Phase, StatusMessage: start.StatusMessage, CurrentStep: start.CurrentStep,
		TotalSteps: start.TotalSteps, ProgressPercent: start.ProgressPercent, CurrentCommand: start.CurrentCommand,
		WaitReason: start.WaitReason, GitHubRateSnapshotJSON: runtimeFixtureJSON(start.GitHubRateSnapshotJSON), CIState: start.CIState,
		CapacitySnapshotJSON: runtimeFixtureJSON(start.CapacitySnapshotJSON), WorkerMetadataJSON: runtimeFixtureJSON(start.WorkerMetadataJSON),
		MetricsJSON: runtimeFixtureJSON(start.MetricsJSON), NextAction: start.NextAction, DetentSessionID: max(start.DetentSessionID, 0),
		ProviderSessionID: start.ProviderSessionID, RuntimeIdentity: start.RuntimeIdentity.Normalize(),
	}
	f.attempts.attempts = append(f.attempts.attempts, attempt)
	return attempt.ID, nil
}

func (f *runtimeFixture) WorkAttempt(ctx context.Context, id int64) (store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return store.WorkAttempt{}, err
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	attempt := f.find(id)
	if attempt == nil {
		return store.WorkAttempt{}, store.ErrNotFound
	}
	return *attempt, nil
}

func (f *runtimeFixture) RecordWorkAttemptHeartbeat(ctx context.Context, heartbeat store.WorkAttemptHeartbeat) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if heartbeat.AttemptID <= 0 {
		return errors.New("attempt_id is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	attempt := f.find(heartbeat.AttemptID)
	if attempt == nil {
		return fmt.Errorf("%w: work attempt %d", store.ErrNotFound, heartbeat.AttemptID)
	}
	if !attempt.CompletedAt.IsZero() {
		return fmt.Errorf("%w: %d", store.ErrWorkAttemptTerminal, heartbeat.AttemptID)
	}
	attempt.HeartbeatAt, attempt.LeaseExpiresAt = runtimeFixtureTime(heartbeat.HeartbeatAt), runtimeFixtureTime(heartbeat.LeaseExpiresAt)
	attempt.Phase, attempt.StatusMessage = heartbeat.Phase, heartbeat.StatusMessage
	attempt.CurrentStep, attempt.TotalSteps, attempt.ProgressPercent = heartbeat.CurrentStep, heartbeat.TotalSteps, heartbeat.ProgressPercent
	attempt.CurrentCommand, attempt.WaitReason, attempt.CIState = heartbeat.CurrentCommand, heartbeat.WaitReason, heartbeat.CIState
	attempt.GitHubRateSnapshotJSON, attempt.CapacitySnapshotJSON = runtimeFixtureJSON(heartbeat.GitHubRateSnapshotJSON), runtimeFixtureJSON(heartbeat.CapacitySnapshotJSON)
	attempt.WorkerMetadataJSON, attempt.MetricsJSON = runtimeFixtureJSON(heartbeat.WorkerMetadataJSON), runtimeFixtureJSON(heartbeat.MetricsJSON)
	attempt.NextAction, attempt.ErrorClass, attempt.ErrorMessage = heartbeat.NextAction, heartbeat.ErrorClass, heartbeat.ErrorMessage
	f.keepSession(attempt, heartbeat.DetentSessionID, heartbeat.ProviderSessionID, heartbeat.RuntimeIdentity)
	return nil
}

func (f *runtimeFixture) keepSession(attempt *store.WorkAttempt, sessionID int64, providerSessionID string, identity agentidentity.Identity) {
	if sessionID > 0 {
		attempt.DetentSessionID = sessionID
	}
	if strings.TrimSpace(providerSessionID) != "" {
		attempt.ProviderSessionID = providerSessionID
	}
	if normalized := identity.Normalize(); normalized != (agentidentity.Identity{}) {
		attempt.RuntimeIdentity = normalized
	}
}

func (f *runtimeFixture) CompleteWorkAttempt(ctx context.Context, completion store.WorkAttemptCompletion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if completion.AttemptID <= 0 {
		return errors.New("attempt_id is required")
	}
	if completion.CompletedAt.IsZero() {
		return errors.New("completed_at is required")
	}
	if completion.Status != "" && completion.Status != store.WorkAttemptStatusTerminal {
		return fmt.Errorf("status = %q, want %q", completion.Status, store.WorkAttemptStatusTerminal)
	}
	if completion.TerminalState == "" {
		return errors.New("terminal_state is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	attempt := f.find(completion.AttemptID)
	if attempt == nil || !attempt.CompletedAt.IsZero() {
		return fmt.Errorf("%w: work attempt %d", store.ErrNotFound, completion.AttemptID)
	}
	completedAt := runtimeFixtureTime(completion.CompletedAt)
	attempt.Status, attempt.TerminalState = store.WorkAttemptStatusTerminal, completion.TerminalState
	attempt.CompletedAt, attempt.HeartbeatAt, attempt.LeaseExpiresAt = completedAt, completedAt, time.Time{}
	attempt.ErrorClass, attempt.ErrorMessage = completion.ErrorClass, completion.ErrorMessage
	attempt.Phase, attempt.StatusMessage, attempt.WaitReason = completion.Phase, completion.StatusMessage, completion.WaitReason
	attempt.GitHubRateSnapshotJSON, attempt.CIState = runtimeFixtureJSON(completion.GitHubRateSnapshotJSON), completion.CIState
	attempt.CapacitySnapshotJSON = runtimeFixtureJSON(completion.CapacitySnapshotJSON)
	attempt.WorkerMetadataJSON, attempt.MetricsJSON = runtimeFixtureJSON(completion.WorkerMetadataJSON), runtimeFixtureJSON(completion.MetricsJSON)
	attempt.NextAction = completion.NextAction
	f.keepSession(attempt, completion.DetentSessionID, completion.ProviderSessionID, completion.RuntimeIdentity)
	return nil
}

func (f *runtimeFixture) UpdateTerminalWorkAttemptWait(ctx context.Context, update store.WorkAttemptTerminalWaitUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if update.AttemptID <= 0 {
		return errors.New("attempt_id is required")
	}
	if strings.TrimSpace(update.ExpectedErrorClass) == "" {
		return errors.New("expected_error_class is required")
	}
	if update.TerminalState == "" {
		return errors.New("terminal_state is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	attempt := f.find(update.AttemptID)
	if attempt == nil || !runtimeFixtureTerminal(*attempt) || attempt.ErrorClass != update.ExpectedErrorClass {
		return fmt.Errorf("%w: work attempt %d", store.ErrNotFound, update.AttemptID)
	}
	attempt.TerminalState, attempt.ErrorClass, attempt.ErrorMessage = update.TerminalState, update.ErrorClass, update.ErrorMessage
	attempt.Phase, attempt.StatusMessage = update.Phase, update.StatusMessage
	attempt.WorkerMetadataJSON = runtimeFixtureJSON(update.WorkerMetadataJSON)
	return nil
}

func (f *runtimeFixture) ListActiveWorkAttempts(ctx context.Context, query store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	project := strings.TrimSpace(query.ProjectID)
	active := make([]store.WorkAttempt, 0)
	for _, attempt := range f.attempts.attempts {
		if attempt.CompletedAt.IsZero() && (project == "" || attempt.ProjectID == project) {
			active = append(active, attempt)
		}
	}
	slices.SortStableFunc(active, func(left, right store.WorkAttempt) int {
		if order := left.StartedAt.Compare(right.StartedAt); order != 0 {
			return order
		}
		return cmp.Compare(left.ID, right.ID)
	})
	return active, nil
}

func (f *runtimeFixture) ListRecentTerminalWorkAttempts(ctx context.Context, query store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	project, workerType := strings.TrimSpace(query.ProjectID), strings.TrimSpace(query.WorkerType)
	issueID, identifier, issueURL := strings.TrimSpace(query.IssueID), strings.TrimSpace(query.Identifier), strings.TrimSpace(query.IssueURL)
	recent := make([]store.WorkAttempt, 0)
	for _, attempt := range f.attempts.attempts {
		if !runtimeFixtureTerminal(attempt) || runtimeFixtureOutcomeExcluded(attempt) {
			continue
		}
		if project != "" && attempt.ProjectID != project || workerType != "" && attempt.WorkerType != workerType {
			continue
		}
		unfiltered := issueID == "" && identifier == "" && issueURL == ""
		if !unfiltered && (issueID == "" || attempt.IssueID != issueID) && (identifier == "" || attempt.Identifier != identifier) && (issueURL == "" || attempt.IssueURL != issueURL) {
			continue
		}
		recent = append(recent, attempt)
	}
	slices.SortStableFunc(recent, func(left, right store.WorkAttempt) int { return runtimeFixtureCompletedBefore(right, left) })
	return recent[:min(limit, len(recent))], nil
}

func (f *runtimeFixture) ListPendingForgeAvailabilityWaits(ctx context.Context, projectID string) ([]store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, errors.New("project_id is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	const credentialClass = "worker_github_credential_unavailable"
	pending := make([]store.WorkAttempt, 0)
	for _, waiting := range f.attempts.attempts {
		present, host, waitClass, _ := runtimeFixtureForgeWait(waiting)
		if waiting.ProjectID != projectID || !runtimeFixtureTerminal(waiting) || waiting.TerminalState != store.WorkAttemptTerminalCapacity ||
			waiting.ErrorClass != "forge_unavailable" || strings.TrimSpace(waiting.IssueID) == "" || !present || runtimeFixtureOutcomeExcluded(waiting) {
			continue
		}
		superseded := false
		for _, newer := range f.attempts.attempts {
			if newer.ProjectID != waiting.ProjectID || !runtimeFixtureTerminal(newer) || runtimeFixtureOutcomeExcluded(newer) || runtimeFixtureCompletedBefore(newer, waiting) <= 0 {
				continue
			}
			_, _, newerClass, completedHost := runtimeFixtureForgeWait(newer)
			sameIssue := newer.IssueID == waiting.IssueID && (waitClass != credentialClass ||
				newer.TerminalState == store.WorkAttemptTerminalCapacity && newer.ErrorClass == "forge_unavailable" && newerClass == credentialClass)
			if completedHost != "" && completedHost == host || sameIssue {
				superseded = true
				break
			}
		}
		if !superseded {
			pending = append(pending, waiting)
		}
	}
	slices.SortStableFunc(pending, runtimeFixtureCompletedBefore)
	return pending, nil
}

func (f *runtimeFixture) ListPendingWorkAttemptCapacityReleases(ctx context.Context, projectID string) ([]store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, errors.New("project_id is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	pending := make([]store.WorkAttempt, 0)
	for _, attempt := range f.attempts.attempts {
		if attempt.ProjectID == projectID && runtimeFixtureTerminal(attempt) && strings.EqualFold(strings.TrimSpace(attempt.NextAction), "release capacity") {
			pending = append(pending, attempt)
		}
	}
	slices.SortStableFunc(pending, runtimeFixtureCompletedBefore)
	return pending, nil
}

func (f *runtimeFixture) ClearWorkAttemptCapacityRelease(ctx context.Context, attemptID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if attemptID <= 0 {
		return errors.New("attempt_id is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	if attempt := f.find(attemptID); attempt != nil && runtimeFixtureTerminal(*attempt) && strings.EqualFold(strings.TrimSpace(attempt.NextAction), "release capacity") {
		attempt.NextAction = ""
	}
	return nil
}

func (f *runtimeFixture) UpdateOperatorStop(ctx context.Context, update store.OperatorStopUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	attempt := f.find(update.AttemptID)
	if attempt == nil || attempt.TerminalState != store.WorkAttemptTerminalOperatorStopped {
		return store.ErrNotFound
	}
	attempt.Phase, attempt.StatusMessage, attempt.NextAction = update.Phase, update.StatusMessage, update.NextAction
	attempt.WorkerMetadataJSON = update.WorkerMetadataJSON
	return nil
}

func (f *runtimeFixture) ListPendingOperatorStops(ctx context.Context, projectID string) ([]store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	pending := make([]store.WorkAttempt, 0)
	for _, attempt := range f.attempts.attempts {
		if attempt.ProjectID == strings.TrimSpace(projectID) && attempt.TerminalState == store.WorkAttemptTerminalOperatorStopped &&
			(attempt.Phase == "operator_stop_pending" || attempt.Phase == "operator_stop_transition_failed") {
			pending = append(pending, attempt)
		}
	}
	slices.SortStableFunc(pending, runtimeFixtureCompletedBefore)
	return pending, nil
}

func (f *runtimeFixture) TimeoutExpiredWorkAttempts(ctx context.Context, timeout store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout.Now.IsZero() {
		return nil, errors.New("now is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	now := runtimeFixtureTime(timeout.Now)
	terminalState, errorClass, errorMessage := timeout.TerminalState, timeout.ErrorClass, timeout.ErrorMessage
	if terminalState == "" {
		terminalState = store.WorkAttemptTerminalTimedOut
	}
	if strings.TrimSpace(errorClass) == "" {
		errorClass = "lease_expired"
	}
	if strings.TrimSpace(errorMessage) == "" {
		errorMessage = "work attempt lease expired"
	}
	project := strings.TrimSpace(timeout.ProjectID)
	timedOut := make([]store.WorkAttempt, 0)
	for index := range f.attempts.attempts {
		attempt := &f.attempts.attempts[index]
		deferred := strings.EqualFold(strings.TrimSpace(attempt.Phase), "completion_deferred")
		expired := !attempt.LeaseExpiresAt.IsZero() && !attempt.LeaseExpiresAt.After(now) ||
			attempt.LeaseExpiresAt.IsZero() && deferred || !deferred && slices.Contains(timeout.ConfirmedGoneAttemptIDs, attempt.ID)
		if !attempt.CompletedAt.IsZero() || project != "" && attempt.ProjectID != project || !expired || slices.Contains(timeout.ExcludeAttemptIDs, attempt.ID) {
			continue
		}
		attempt.Status, attempt.TerminalState = store.WorkAttemptStatusTerminal, terminalState
		attempt.CompletedAt, attempt.HeartbeatAt = now, now
		attempt.ErrorClass, attempt.ErrorMessage, attempt.Phase, attempt.StatusMessage = errorClass, errorMessage, "recovered", errorMessage
		timedOut = append(timedOut, *attempt)
	}
	return timedOut, nil
}

func (f *runtimeFixture) RecordSchedulerDecision(ctx context.Context, decision store.SchedulerDecision) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	decision.ProjectID = strings.TrimSpace(decision.ProjectID)
	if decision.ProjectID == "" {
		return 0, errors.New("project_id is required")
	}
	if decision.DecisionAt.IsZero() {
		return 0, errors.New("decision_at is required")
	}
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	if decision.Result == "" {
		decision.Result = store.SchedulerDecisionResultSkipped
		if decision.Selected {
			decision.Result = store.SchedulerDecisionResultSelected
		}
	}
	decision.Selected = decision.Selected || decision.Result == store.SchedulerDecisionResultSelected
	decision.DecisionAt = runtimeFixtureTime(decision.DecisionAt)
	decision.QueuePosition, decision.AttemptNumber = max(decision.QueuePosition, 0), max(decision.AttemptNumber, 0)
	decision.CapacitySnapshotJSON, decision.GitHubRateSnapshotJSON = runtimeFixtureJSON(decision.CapacitySnapshotJSON), runtimeFixtureJSON(decision.GitHubRateSnapshotJSON)
	decision.MetadataJSON = runtimeFixtureJSON(decision.MetadataJSON)
	decision.ID = int64(len(f.attempts.decisions) + 1)
	f.attempts.decisions = append(f.attempts.decisions, decision)
	return decision.ID, nil
}

func (f *runtimeFixture) recentDecisions(limit int, match func(store.SchedulerDecision) bool) []store.SchedulerDecision {
	f.attempts.mu.Lock()
	defer f.attempts.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	decisions := make([]store.SchedulerDecision, 0)
	for _, decision := range f.attempts.decisions {
		if match(decision) {
			decisions = append(decisions, decision)
		}
	}
	slices.SortStableFunc(decisions, func(left, right store.SchedulerDecision) int {
		if order := right.DecisionAt.Compare(left.DecisionAt); order != 0 {
			return order
		}
		return cmp.Compare(right.ID, left.ID)
	})
	return decisions[:min(limit, len(decisions))]
}

func (f *runtimeFixture) ListRecentSchedulerDecisions(ctx context.Context, query store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	project := strings.TrimSpace(query.ProjectID)
	return f.recentDecisions(query.Limit, func(decision store.SchedulerDecision) bool {
		return project == "" || decision.ProjectID == project
	}), nil
}

func (f *runtimeFixture) ListIssueSchedulerDecisions(ctx context.Context, query store.IssueSchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	project, issueID := strings.TrimSpace(query.Identity.ProjectID), strings.TrimSpace(query.Identity.IssueID)
	identifier, issueURL := strings.TrimSpace(query.Identity.Identifier), strings.TrimSpace(query.Identity.IssueURL)
	if project == "" {
		return nil, store.ErrProjectRequired
	}
	if issueID == "" && identifier == "" && issueURL == "" {
		return []store.SchedulerDecision{}, nil
	}
	return f.recentDecisions(query.Limit, func(decision store.SchedulerDecision) bool {
		return decision.ProjectID == project && (issueID != "" && decision.IssueID == issueID ||
			identifier != "" && decision.Identifier == identifier || issueURL != "" && decision.IssueURL == issueURL)
	}), nil
}

func (f *runtimeFixture) LatestAgentResumeState(context.Context, store.AgentResumeLookup) (store.AgentResumeState, error) {
	return store.AgentResumeState{}, store.ErrNotFound
}

func (f *runtimeFixture) LatestIssueAgentResumeState(ctx context.Context, identity store.IssueIdentity) (store.AgentResumeState, error) {
	if err := ctx.Err(); err != nil {
		return store.AgentResumeState{}, err
	}
	if strings.TrimSpace(identity.ProjectID) == "" {
		return store.AgentResumeState{}, store.ErrProjectRequired
	}
	if f.resume.DetentSessionID == 0 || identity.IssueID == "" && identity.Identifier == "" && identity.IssueURL == "" {
		return store.AgentResumeState{}, store.ErrNotFound
	}
	return f.resume, nil
}

func (f *runtimeFixture) LaneWriteLock() sync.Locker { return &f.ledger.lock }

func (f *runtimeFixture) PrepareLaneWrite(ctx context.Context, project string, write coordination.LaneWrite) (coordination.LaneWrite, error) {
	if err := ctx.Err(); err != nil {
		return coordination.LaneWrite{}, err
	}
	if strings.TrimSpace(project) == "" || strings.TrimSpace(write.Issue) == "" || strings.TrimSpace(write.To) == "" || strings.TrimSpace(write.Reason) == "" {
		return coordination.LaneWrite{}, errors.New("lane write project, issue, target and reason are required")
	}
	if write.WrittenAt.IsZero() {
		return coordination.LaneWrite{}, errors.New("written_at is required")
	}
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	write.WrittenAt = runtimeFixtureTime(write.WrittenAt)
	write.InstanceIdentity = "runtime-fixture"
	write.FenceToken = uint64(len(f.ledger.writes) + 1)
	f.ledger.writes = append(f.ledger.writes, runtimeFixtureLaneWrite{project: project, write: write, result: "prepared"})
	return write, nil
}

func (f *runtimeFixture) laneWrite(token uint64) *runtimeFixtureLaneWrite {
	if token == 0 || token > uint64(len(f.ledger.writes)) {
		return nil
	}
	return &f.ledger.writes[token-1]
}

func (f *runtimeFixture) ResolveLaneWrite(ctx context.Context, token uint64, result string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if token == 0 || !slices.Contains([]string{"applied", "failed", "blocked", "uncertain"}, result) {
		return errors.New("lane write token and valid result are required")
	}
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	write := f.laneWrite(token)
	if write == nil {
		return fmt.Errorf("%w: lane write %d", store.ErrNotFound, token)
	}
	write.result = result
	return nil
}

func (f *runtimeFixture) RecordLaneWriteAction(ctx context.Context, token uint64, origin, kind string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if token == 0 || strings.TrimSpace(origin) == "" {
		return errors.New("lane write token and origin are required")
	}
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	write := f.laneWrite(token)
	if write == nil {
		return fmt.Errorf("%w: lane write %d", store.ErrNotFound, token)
	}
	write.origin, write.kind = strings.TrimSpace(origin), strings.TrimSpace(kind)
	return nil
}

func (f *runtimeFixture) LatestLaneWrite(ctx context.Context, issue store.IssueIdentity) (coordination.LaneWrite, string, error) {
	if err := ctx.Err(); err != nil {
		return coordination.LaneWrite{}, "", err
	}
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	for index := len(f.ledger.writes) - 1; index >= 0; index-- {
		if write := f.ledger.writes[index]; write.project == issue.ProjectID && write.write.Issue == issue.IssueID {
			return write.write, write.result, nil
		}
	}
	return coordination.LaneWrite{}, "", store.ErrNotFound
}

func (f *runtimeFixture) LaneObservation(ctx context.Context, issue store.IssueIdentity) (store.LaneObservation, error) {
	if err := ctx.Err(); err != nil {
		return store.LaneObservation{}, err
	}
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	observation, ok := f.ledger.observations[store.IssueIdentity{ProjectID: issue.ProjectID, IssueID: issue.IssueID}]
	if !ok {
		return store.LaneObservation{}, store.ErrNotFound
	}
	return observation, nil
}

func (f *runtimeFixture) SaveLaneObservation(ctx context.Context, issue store.IssueIdentity, observation store.LaneObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(issue.ProjectID) == "" || strings.TrimSpace(issue.IssueID) == "" || strings.TrimSpace(observation.State) == "" {
		return errors.New("lane observation project, issue and state are required")
	}
	if observation.EnteredAt.IsZero() {
		return errors.New("entered_at is required")
	}
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	if f.ledger.observations == nil {
		f.ledger.observations = map[store.IssueIdentity]store.LaneObservation{}
	}
	observation.EnteredAt = runtimeFixtureTime(observation.EnteredAt)
	f.ledger.observations[store.IssueIdentity{ProjectID: issue.ProjectID, IssueID: issue.IssueID}] = observation
	return nil
}

type laneMutationHistory interface {
	WorkflowMetricsRecorder
	store.LaneLedgerStore
}

type laneMutationRecorder struct {
	autoPromoteWorkflowMetricsRecorder
	recordingLaneLedger
}

func newLaneMutationTestOrchestrator(
	cfg Config,
	tracker connector.Connector,
	runtimeStore laneMutationHistory,
	attempts store.WorkAttemptStore,
	now time.Time,
) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		workflowMetrics: runtimeStore,
		workAttempts:    attempts,
		laneLedger:      runtimeStore,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:             func() time.Time { return now },
	}
}
