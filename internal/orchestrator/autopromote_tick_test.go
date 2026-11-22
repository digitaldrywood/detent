package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func autoPromoteTickIssue(id string, labels []string, pullRequest *connector.PullRequest) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#42"
	issue.Title = "Auto promote test"
	issue.State = "Human Review"
	issue.Labels = append([]string(nil), labels...)
	issue.PullRequest = pullRequest
	return issue
}

func autoPromoteTickStatesEqual(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, state := range got {
		seen[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	for _, state := range want {
		if _, ok := seen[strings.ToLower(strings.TrimSpace(state))]; !ok {
			return false
		}
	}
	return true
}

type autoPromoteTickUpdate struct {
	issueID string
	state   string
}

type autoPromoteTickComment struct {
	issueID string
	body    string
}

type autoPromoteTickSetField struct {
	issueID string
	field   string
	value   string
}

type autoPromoteTickMerge struct {
	repository string
	number     int
	headSHA    string
	method     string
}

type autoPromoteTickRerun struct {
	issueID string
	checks  []connector.PullRequestCheck
}

type autoPromoteTickHydration struct {
	issueID    string
	repository string
	number     int
}

type autoPromoteTickRelabel struct {
	repository string
	number     int
	label      string
	stagger    time.Duration
}

func autoPromoteReworkEventMetadata(prNumber int, headSHA string, failedChecks ...string) string {
	issue := connector.Issue{
		PullRequest: &connector.PullRequest{
			Number:  prNumber,
			HeadSHA: headSHA,
		},
	}
	for _, check := range failedChecks {
		issue.PullRequest.RequiredCheckFailures = append(issue.PullRequest.RequiredCheckFailures, connector.PullRequestCheck{
			Name:       check,
			Status:     "completed",
			Conclusion: "failure",
		})
	}
	return workflowLaneMetadataJSON(issue, workflowLaneMetadata{})
}

type autoPromoteWorkflowMetricsRecorder struct {
	mu     sync.Mutex
	events []store.WorkflowPhaseEvent
}

func (r *autoPromoteWorkflowMetricsRecorder) RecordWorkflowPhaseEvent(_ context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.ID <= 0 {
		event.ID = int64(len(r.events) + 1)
	}
	r.events = append(r.events, event)
	return event.ID, nil
}

func (r *autoPromoteWorkflowMetricsRecorder) UpdateWorkflowPhaseEventMetadata(_ context.Context, eventID int64, metadataJSON string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for index := range r.events {
		if r.events[index].ID == eventID {
			r.events[index].MetadataJSON = metadataJSON
			return nil
		}
	}
	return store.ErrNotFound
}

func (r *autoPromoteWorkflowMetricsRecorder) IssueWorkflowTimeline(_ context.Context, identity store.IssueIdentity) (store.WorkflowTimeline, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]store.WorkflowPhaseEvent, 0, len(r.events))
	for _, event := range r.events {
		if event.IssueID != "" && event.IssueID == identity.IssueID {
			events = append(events, event)
			continue
		}
		if event.Identifier != "" && event.Identifier == identity.Identifier {
			events = append(events, event)
			continue
		}
		if event.IssueURL != "" && event.IssueURL == identity.IssueURL {
			events = append(events, event)
		}
	}
	return store.WorkflowTimeline{Events: events}, nil
}

func (r *autoPromoteWorkflowMetricsRecorder) snapshot() []store.WorkflowPhaseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]store.WorkflowPhaseEvent(nil), r.events...)
}

type autoPromoteTickConnector struct {
	stateIssues             []connector.Issue
	candidateIssues         []connector.Issue
	candidateIssuesSet      bool
	candidateByStates       [][]string
	fetchByStatesRequests   [][]string
	fetchComments           []string
	fetchIdentifiers        [][]string
	issueComments           map[string][]connector.IssueComment
	resolvedIssues          []connector.Issue
	updates                 []autoPromoteTickUpdate
	comments                []autoPromoteTickComment
	prComments              []autoPromoteTickComment
	setFields               []autoPromoteTickSetField
	reruns                  []autoPromoteTickRerun
	updateErr               error
	reviewThreadHydrations  []string
	reviewThreadUnavailable string
}

type autoPromoteTickMergeConnector struct {
	afterHydrate func()
	afterMerge   func()
	*autoPromoteTickConnector
	merges         []autoPromoteTickMerge
	hydrations     []autoPromoteTickHydration
	relabels       []autoPromoteTickRelabel
	relabelStarted chan autoPromoteTickRelabel
	relabelRelease <-chan struct{}
	hydratedIssues []connector.Issue
	err            error
	hydrateErr     error
}

func (c *autoPromoteTickConnector) Name() string {
	return "auto-promote-tick"
}

func (c *autoPromoteTickConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	return c.FetchCandidateIssuesByStates(ctx, []string{"Todo", "In Progress", "Rework", "Merging"})
}

func (c *autoPromoteTickConnector) FetchCandidateIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.candidateByStates = append(c.candidateByStates, append([]string(nil), states...))
	if c.candidateIssuesSet {
		return issuesInStates(c.candidateIssues, states), nil
	}
	return issuesInStates(c.stateIssues, states), nil
}

func (c *autoPromoteTickConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetchByStatesRequests = append(c.fetchByStatesRequests, append([]string(nil), states...))
	wanted := make(map[string]struct{}, len(states))
	for _, state := range states {
		wanted[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.State))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) FetchIssueComments(_ context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	c.fetchComments = append(c.fetchComments, strings.TrimSpace(issue.ID))
	return cloneIssueComments(c.issueComments[strings.TrimSpace(issue.ID)]), nil
}

func (c *autoPromoteTickConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[issue.ID]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) ([]connector.Issue, error) {
	c.fetchIdentifiers = append(c.fetchIdentifiers, append([]string(nil), identifiers...))
	wanted := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		wanted[strings.ToLower(strings.TrimSpace(identifier))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.resolvedIssues))
	for _, issue := range c.resolvedIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.Identifier))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) HydratePullRequestReviewThreads(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.reviewThreadHydrations = append(c.reviewThreadHydrations, strings.TrimSpace(issue.ID))
	issue = cloneIssue(issue)
	if c.reviewThreadUnavailable != "" && issue.PullRequest != nil {
		issue.PullRequest.HydrationUnavailableReason = c.reviewThreadUnavailable
	}
	return issue, nil
}

func (c *autoPromoteTickConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, autoPromoteTickComment{issueID: issueID, body: body})
	return nil
}

func (c *autoPromoteTickConnector) CreatePullRequestComment(_ context.Context, repository string, number int, body string) error {
	c.prComments = append(c.prComments, autoPromoteTickComment{issueID: repository, body: body})
	return nil
}

func (c *autoPromoteTickConnector) RerunPullRequestChecks(_ context.Context, issue connector.Issue, checks []connector.PullRequestCheck) error {
	c.reruns = append(c.reruns, autoPromoteTickRerun{
		issueID: issue.ID,
		checks:  append([]connector.PullRequestCheck(nil), checks...),
	})
	return nil
}

func (c *autoPromoteTickMergeConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	if c.afterHydrate != nil {
		defer c.afterHydrate()
	}
	c.hydrations = append(c.hydrations, autoPromoteTickHydration{
		issueID:    issue.ID,
		repository: pullRequestRepository(issue),
		number:     pullRequestNumber(issue),
	})
	if c.hydrateErr != nil {
		return cloneIssue(issue), c.hydrateErr
	}
	issues := c.hydratedIssues
	if len(issues) == 0 && c.autoPromoteTickConnector != nil {
		issues = c.stateIssues
	}
	for _, candidate := range issues {
		if strings.TrimSpace(candidate.ID) != "" && strings.TrimSpace(candidate.ID) == strings.TrimSpace(issue.ID) {
			return cloneIssue(candidate), nil
		}
		if strings.TrimSpace(candidate.Identifier) != "" && strings.EqualFold(strings.TrimSpace(candidate.Identifier), strings.TrimSpace(issue.Identifier)) {
			return cloneIssue(candidate), nil
		}
		if pullRequestNumber(candidate) > 0 &&
			pullRequestNumber(candidate) == pullRequestNumber(issue) &&
			strings.EqualFold(pullRequestRepository(candidate), pullRequestRepository(issue)) {
			return cloneIssue(candidate), nil
		}
	}
	return cloneIssue(issue), nil
}

func (c *autoPromoteTickMergeConnector) ReapplyPullRequestLabel(ctx context.Context, repository string, number int, label string, stagger time.Duration) error {
	relabel := autoPromoteTickRelabel{
		repository: repository,
		number:     number,
		label:      label,
		stagger:    stagger,
	}
	c.relabels = append(c.relabels, relabel)
	if c.relabelStarted != nil {
		c.relabelStarted <- relabel
	}
	if c.relabelRelease != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.relabelRelease:
		}
	}
	return nil
}

func (c *autoPromoteTickMergeConnector) MergePullRequest(_ context.Context, repository string, number int, headSHA string, method string) error {
	if c.afterMerge != nil {
		defer c.afterMerge()
	}
	c.merges = append(c.merges, autoPromoteTickMerge{repository: repository, number: number, headSHA: headSHA, method: method})
	if c.err != nil {
		return c.err
	}
	for index := range c.stateIssues {
		issue := &c.stateIssues[index]
		if pullRequestNumber(*issue) != number || !strings.EqualFold(pullRequestRepository(*issue), repository) || issue.PullRequest == nil {
			continue
		}
		issue.PullRequest.State = "MERGED"
		now := time.Date(2026, 6, 26, 13, 15, 31, 0, time.UTC)
		issue.PullRequest.ActivityAt = &now
		issue.UpdatedAt = &now
		return nil
	}
	return nil
}

type autoPromoteTickValidator struct {
	mu       sync.Mutex
	result   gate.ValidatorResult
	requests []ValidatorRequest
	err      error
}

func (v *autoPromoteTickValidator) Validate(_ context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = append(v.requests, req)
	return v.result, v.err
}

type blockingAutoPromoteValidatorRunner struct {
	err         error
	releaseOnce sync.Once
	started     chan ValidatorRequest
	runStarted  chan RunRequest
	canceled    chan struct{}
	release     chan struct{}
	done        chan struct{}
}

func newBlockingAutoPromoteValidatorRunner() *blockingAutoPromoteValidatorRunner {
	return &blockingAutoPromoteValidatorRunner{
		started:    make(chan ValidatorRequest, 1),
		runStarted: make(chan RunRequest, 1),
		canceled:   make(chan struct{}),
		release:    make(chan struct{}),
		done:       make(chan struct{}),
	}
}

func (r *blockingAutoPromoteValidatorRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	r.runStarted <- req
	<-ctx.Done()
	return RunResult{FinalState: runpkg.FinalStateFailed}, ctx.Err()
}

func (r *blockingAutoPromoteValidatorRunner) Validate(ctx context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	r.started <- req
	defer close(r.done)

	select {
	case <-r.release:
		if r.err != nil {
			return gate.ValidatorResult{}, r.err
		}
		return gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1}, nil
	case <-ctx.Done():
		close(r.canceled)
		<-r.release
		return gate.ValidatorResult{}, ctx.Err()
	}
}

func (r *blockingAutoPromoteValidatorRunner) Release() {
	r.releaseOnce.Do(func() {
		close(r.release)
	})
}

func (v *autoPromoteTickValidator) Requests() []ValidatorRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]ValidatorRequest(nil), v.requests...)
}

func waitForValidatorRequests(t *testing.T, validator *autoPromoteTickValidator, count int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(validator.Requests()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator requests = %d, want at least %d", len(validator.Requests()), count)
}

func waitForValidatorResult(t *testing.T, orch *Orchestrator, issue connector.Issue) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := orch.validatorStageResult(context.Background(), issue); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("validator result was not recorded")
}

func waitForPersistedValidatorVerdict(t *testing.T, memo store.ValidatorMemoStore, key store.ValidatorVerdictKey) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := memo.ValidatorVerdict(context.Background(), key); err == nil {
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ValidatorVerdict() error = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator verdict was not persisted for %#v", key)
}

func waitForPersistedValidatorFailure(t *testing.T, memo store.ValidatorMemoStore, key store.ValidatorVerdictKey, attempt int) store.ValidatorVerdict {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		verdict, err := memo.ValidatorVerdict(t.Context(), key)
		if err == nil && verdict.FailureAttempts >= attempt {
			return verdict
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ValidatorVerdict() error = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("persisted validator failure attempt %d was not recorded", attempt)
	return store.ValidatorVerdict{}
}

func waitForValidatorFailure(t *testing.T, orch *Orchestrator, issue connector.Issue, attempt int) validatorStageFailure {
	t.Helper()

	identity := validatorStageIdentityForIssue(issue)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		orch.validatorMu.Lock()
		failure, ok := orch.validatorFailures[identity.Key]
		orch.validatorMu.Unlock()
		if ok && failure.Attempt >= attempt {
			return failure
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator failure attempt %d was not recorded", attempt)
	return validatorStageFailure{}
}

func openValidatorMemoStore(t *testing.T) store.ValidatorMemoStore {
	t.Helper()

	return &validatorMemoFixture{}
}

func autoPromoteValidatorTestConfig() Config {
	return normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate: gate.Config{
				Kind: gate.KindCommand,
				Validator: gate.ValidatorConfig{
					Enabled:  true,
					MinScore: 0.8,
					BlockOn:  []string{"p1"},
				},
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
}

type autoPromoteTickClock struct {
	mu  sync.Mutex
	now time.Time
}

func newAutoPromoteTickClock(now time.Time) *autoPromoteTickClock {
	return &autoPromoteTickClock{now: now}
}

func (c *autoPromoteTickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *autoPromoteTickClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func waitForGlobalDispatchSlot(t *testing.T, globalGate scheduler.ProjectDispatchGate, projectID string) {
	t.Helper()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		slot, ok, err := globalGate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: projectID}, scheduler.SlotRequest{State: "Todo"}, time.Now())
		if err != nil {
			t.Fatalf("TryAcquire() error = %v", err)
		}
		if ok {
			if err := globalGate.Release(slot); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
			return
		}

		select {
		case <-deadline:
			t.Fatal("global dispatch slot was not released before validator drain completed")
		case <-ticker.C:
		}
	}
}

func (c *autoPromoteTickConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, autoPromoteTickUpdate{issueID: issueID, state: state})
	if c.updateErr != nil {
		return c.updateErr
	}
	for index := range c.stateIssues {
		if c.stateIssues[index].ID == issueID {
			c.stateIssues[index].State = state
		}
	}
	return nil
}

func (c *autoPromoteTickConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *autoPromoteTickConnector) SetField(_ context.Context, issueID string, field string, value string) error {
	c.setFields = append(c.setFields, autoPromoteTickSetField{issueID: issueID, field: field, value: value})
	for index := range c.stateIssues {
		if c.stateIssues[index].ID != issueID {
			continue
		}
		if c.stateIssues[index].Fields == nil {
			c.stateIssues[index].Fields = map[string]string{}
		}
		c.stateIssues[index].Fields[field] = value
	}
	return nil
}

func nativeMergeQueueTestConfig(cfg Config) Config {
	cfg.AutoPromote.Gate.Kind = gate.KindArtifact
	return normalizeConfig(cfg)
}

func nativeMergeQueueTestIssue(number int, ciStatus string) connector.Issue {
	issue := autoPromoteTickIssue(fmt.Sprintf("issue-%d", number), []string{"bug"}, &connector.PullRequest{
		NodeID:         fmt.Sprintf("PR_%d", number),
		Number:         number,
		URL:            fmt.Sprintf("https://github.test/digitaldrywood/detent/pull/%d", number),
		BranchName:     fmt.Sprintf("detent/issue-%d", number),
		BaseRef:        "main",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       ciStatus,
		HeadSHA:        fmt.Sprintf("head-%d", number),
	})
	issue.State = "Merging"
	issue.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", number)
	issue.PRRepository = "digitaldrywood/detent"
	return issue
}

func successfulReworkGateWaitAttempt(
	completedAt time.Time,
	issue connector.Issue,
	signature autoPromoteReworkSignature,
	includeMarker bool,
) store.WorkAttempt {
	prNumber := signature.PRNumber
	metadata := implementCompletionProgressMetadata(implementCompletionProgressDecision{
		Outcome:            store.WorkAttemptTerminalSuccess,
		Reason:             "unchanged_signature_clean_diff",
		CurrentSignature:   signature,
		WorkspaceDiffStats: DiffStats{Status: "clean"},
		TrackerState:       issue.State,
	})
	if includeMarker {
		metadata = mergeWorkAttemptMetadata(metadata, completionGateWaitMetadata(completedReworkGateWaitReason, issue))
	}
	return store.WorkAttempt{
		ID:                 2030,
		ProjectID:          "detent",
		IssueID:            issue.ID,
		Identifier:         issue.Identifier,
		IssueURL:           issue.URL,
		PRNumber:           &prNumber,
		WorkerType:         "agent",
		Lane:               "Rework",
		Status:             store.WorkAttemptStatusTerminal,
		StartedAt:          completedAt.Add(-time.Minute),
		CompletedAt:        completedAt,
		TerminalState:      store.WorkAttemptTerminalSuccess,
		WorkerMetadataJSON: marshalWorkAttemptJSON(metadata),
	}
}
