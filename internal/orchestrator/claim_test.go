package orchestrator

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestClaimingOverlappingOrchestratorsDispatchOnlyTieBreakWinner(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	issue := claimTestIssue("issue-1")
	store := newClaimTestStore([]connector.Issue{issue})
	attempts := make(chan string, 2)
	release := make(chan struct{})
	store.assigneeHook = func(ctx context.Context, issueID string, login string) error {
		select {
		case attempts <- login:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alphaRunner := newClaimBlockingRunner()
	betaRunner := newClaimBlockingRunner()
	alpha := newClaimTestOrchestrator(t, claimTestConfig("alpha", "alpha"), claimTestConnector{store: store, login: "alpha"}, alphaRunner)
	beta := newClaimTestOrchestrator(t, claimTestConfig("beta", "beta"), claimTestConnector{store: store, login: "beta"}, betaRunner)
	alphaState := newState(alpha.cfg)
	betaState := newState(beta.cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		alpha.tick(ctx, &alphaState, now)
	}()
	go func() {
		defer wg.Done()
		beta.tick(ctx, &betaState, now)
	}()

	owners := []string{
		receiveClaimAttempt(t, attempts),
		receiveClaimAttempt(t, attempts),
	}
	sort.Strings(owners)
	store.setAssignees(issue.ID, owners)
	close(release)
	wg.Wait()

	started := []RunRequest{}
	if request, ok := receiveOptionalClaimRun(alphaRunner.started); ok {
		started = append(started, request)
	}
	if request, ok := receiveOptionalClaimRun(betaRunner.started); ok {
		started = append(started, request)
	}
	if len(started) != 1 {
		t.Fatalf("started runners = %d, want 1: %#v", len(started), started)
	}
	if started[0].SelectorContext.InstanceLogin != "alpha" {
		t.Fatalf("started owner = %q, want alpha", started[0].SelectorContext.InstanceLogin)
	}
	if _, ok := alphaState.Running[issue.ID]; !ok {
		t.Fatalf("alpha Running[%q] missing", issue.ID)
	}
	if _, ok := betaState.Running[issue.ID]; ok {
		t.Fatalf("beta Running[%q] present", issue.ID)
	}
}

func TestClaimingReclaimsStaleLease(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	issue := claimTestIssue("issue-1")
	issue.AssigneeID = "beta"
	issue.Assignees = []string{"beta"}
	issue.Fields["Detent Lease"] = now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	store := newClaimTestStore([]connector.Issue{issue})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := newClaimBlockingRunner()
	orch := newClaimTestOrchestrator(t, claimTestConfig("alpha", "alpha"), claimTestConnector{store: store, login: "alpha"}, runner)
	state := newState(orch.cfg)

	orch.tick(ctx, &state, now)

	request := receiveClaimRun(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}
	got := store.issue(issue.ID)
	if got.AssigneeID != "alpha" {
		t.Fatalf("AssigneeID = %q, want alpha", got.AssigneeID)
	}
	if got.Fields["Detent Lease"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("Detent Lease = %q, want %q", got.Fields["Detent Lease"], now.Format(time.RFC3339Nano))
	}
	if state.Claimed[issue.ID].Owner != "alpha" {
		t.Fatalf("Claimed owner = %q, want alpha", state.Claimed[issue.ID].Owner)
	}
}

func TestClaimingUsesConfiguredOwnerField(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	issue := claimTestIssue("issue-1")
	issue.Fields["Owner"] = "beta"
	issue.Fields["Detent Lease"] = now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	store := newClaimTestStore([]connector.Issue{issue})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := newClaimBlockingRunner()
	cfg := claimTestConfig("alpha", "alpha")
	cfg.Claiming.OwnershipMode = "field"
	cfg.Claiming.OwnerField = "Owner"
	orch := newClaimTestOrchestrator(t, cfg, claimTestConnector{store: store, login: "alpha"}, runner)
	state := newState(orch.cfg)

	orch.tick(ctx, &state, now)

	receiveClaimRun(t, runner.started)
	got := store.issue(issue.ID)
	if got.Fields["Owner"] != "alpha" {
		t.Fatalf("Owner field = %q, want alpha", got.Fields["Owner"])
	}
	if got.Fields["Detent Lease"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("Detent Lease = %q, want %q", got.Fields["Detent Lease"], now.Format(time.RFC3339Nano))
	}
}

func TestClaimingHeartbeatKeepsActiveLeaseFromReclaim(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	issue := claimTestIssue("issue-1")
	issue.AssigneeID = "alpha"
	issue.Assignees = []string{"alpha"}
	issue.Fields["Detent Lease"] = now.Format(time.RFC3339Nano)
	store := newClaimTestStore([]connector.Issue{issue})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alphaRunner := newClaimBlockingRunner()
	alpha := newClaimTestOrchestrator(t, claimTestConfig("alpha", "alpha"), claimTestConnector{store: store, login: "alpha"}, alphaRunner)
	alphaState := newState(alpha.cfg)
	alpha.tick(ctx, &alphaState, now)
	receiveClaimRun(t, alphaRunner.started)

	heartbeatAt := now.Add(40 * time.Second)
	alpha.tick(ctx, &alphaState, heartbeatAt)
	got := store.issue(issue.ID)
	if got.Fields["Detent Lease"] != heartbeatAt.Format(time.RFC3339Nano) {
		t.Fatalf("Detent Lease after heartbeat = %q, want %q", got.Fields["Detent Lease"], heartbeatAt.Format(time.RFC3339Nano))
	}

	betaRunner := newClaimBlockingRunner()
	beta := newClaimTestOrchestrator(t, claimTestConfig("beta", "beta"), claimTestConnector{store: store, login: "beta"}, betaRunner)
	betaState := newState(beta.cfg)
	beta.tick(ctx, &betaState, now.Add(70*time.Second))

	if _, ok := receiveOptionalClaimRun(betaRunner.started); ok {
		t.Fatalf("beta runner started for active heartbeat")
	}
	if _, ok := betaState.Running[issue.ID]; ok {
		t.Fatalf("beta Running[%q] present", issue.ID)
	}
}

func TestClaimingHeartbeatReleasesLocalRunWhenOwnershipChanges(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	issue := claimTestIssue("issue-1")
	issue.AssigneeID = "alpha"
	issue.Assignees = []string{"alpha"}
	issue.Fields["Detent Lease"] = now.Format(time.RFC3339Nano)
	store := newClaimTestStore([]connector.Issue{issue})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alphaRunner := newClaimBlockingRunner()
	alpha := newClaimTestOrchestrator(t, claimTestConfig("alpha", "alpha"), claimTestConnector{store: store, login: "alpha"}, alphaRunner)
	alphaState := newState(alpha.cfg)
	alpha.tick(ctx, &alphaState, now)
	receiveClaimRun(t, alphaRunner.started)

	reclaimedAt := now.Add(20 * time.Second)
	store.setAssignee(issue.ID, "beta")
	store.setField(issue.ID, "Detent Lease", reclaimedAt.Format(time.RFC3339Nano))

	alpha.tick(ctx, &alphaState, now.Add(40*time.Second))

	got := store.issue(issue.ID)
	if got.AssigneeID != "beta" {
		t.Fatalf("AssigneeID = %q, want beta", got.AssigneeID)
	}
	if got.Fields["Detent Lease"] != reclaimedAt.Format(time.RFC3339Nano) {
		t.Fatalf("Detent Lease = %q, want %q", got.Fields["Detent Lease"], reclaimedAt.Format(time.RFC3339Nano))
	}
	if _, ok := alphaState.Running[issue.ID]; ok {
		t.Fatalf("alpha Running[%q] present after ownership changed", issue.ID)
	}
	if _, ok := alphaState.Claimed[issue.ID]; ok {
		t.Fatalf("alpha Claimed[%q] present after ownership changed", issue.ID)
	}
	receiveClaimCancellation(t, alphaRunner.canceled)
}

func claimTestConfig(owner string, login string) Config {
	return Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		SelectorContext:     selectorContextForClaimTest(login),
		Claiming: ClaimingConfig{
			Enabled:           true,
			OwnershipMode:     "assignee",
			Owner:             owner,
			AssigneeLogin:     login,
			LeaseField:        "Detent Lease",
			LeaseTTL:          time.Minute,
			HeartbeatInterval: 10 * time.Second,
		},
	}
}

func selectorContextForClaimTest(login string) selector.Context {
	return selector.Context{InstanceLogin: login, Persona: login}
}

func claimTestIssue(id string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#1"
	issue.Title = "Claim test"
	issue.State = "Todo"
	return issue
}

type claimTestStore struct {
	mu           sync.Mutex
	issues       map[string]connector.Issue
	assigneeHook func(context.Context, string, string) error
}

func newClaimTestStore(issues []connector.Issue) *claimTestStore {
	out := &claimTestStore{issues: make(map[string]connector.Issue, len(issues))}
	for _, issue := range issues {
		out.issues[issue.ID] = cloneIssue(issue)
	}
	return out
}

func (s *claimTestStore) list() []connector.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()

	issues := make([]connector.Issue, 0, len(s.issues))
	for _, issue := range s.issues {
		issues = append(issues, cloneIssue(issue))
	}
	sort.Slice(issues, func(i, j int) bool {
		return issues[i].ID < issues[j].ID
	})
	return issues
}

func (s *claimTestStore) issue(issueID string) connector.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneIssue(s.issues[issueID])
}

func (s *claimTestStore) setAssignee(issueID string, login string) {
	s.setAssignees(issueID, []string{login})
}

func (s *claimTestStore) setAssignees(issueID string, logins []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	issue := cloneIssue(s.issues[issueID])
	issue.Assignees = append([]string(nil), logins...)
	if len(logins) > 0 {
		issue.AssigneeID = logins[0]
	} else {
		issue.AssigneeID = ""
	}
	s.issues[issueID] = issue
}

func (s *claimTestStore) setField(issueID string, fieldName string, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	issue := cloneIssue(s.issues[issueID])
	if issue.Fields == nil {
		issue.Fields = map[string]string{}
	}
	issue.Fields[fieldName] = value
	s.issues[issueID] = issue
}

type claimTestConnector struct {
	store *claimTestStore
	login string
}

func (c claimTestConnector) Name() string {
	return "claim-test"
}

func (c claimTestConnector) InstanceLogin() string {
	return c.login
}

func (c claimTestConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return c.store.list(), nil
}

func (c claimTestConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	wanted := stateNameSet(states)
	issues := c.store.list()
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := wanted[normalizeState(issue.State)]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (c claimTestConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	wanted := map[string]struct{}{}
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	issues := c.store.list()
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := wanted[issue.ID]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (c claimTestConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c claimTestConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c claimTestConnector) SetAssignee(ctx context.Context, issueID string, login string) error {
	if c.store.assigneeHook != nil {
		return c.store.assigneeHook(ctx, issueID, login)
	}
	c.store.setAssignee(issueID, login)
	return nil
}

func (c claimTestConnector) SetField(_ context.Context, issueID string, fieldName string, value string) error {
	c.store.setField(issueID, fieldName, value)
	return nil
}

type claimBlockingRunner struct {
	started  chan RunRequest
	canceled chan struct{}
}

func newClaimBlockingRunner() *claimBlockingRunner {
	return &claimBlockingRunner{
		started:  make(chan RunRequest, 1),
		canceled: make(chan struct{}, 1),
	}
}

func (r *claimBlockingRunner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	select {
	case r.started <- request:
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}
	<-ctx.Done()
	select {
	case r.canceled <- struct{}{}:
	default:
	}
	return RunResult{}, ctx.Err()
}

func newClaimTestOrchestrator(t *testing.T, cfg Config, conn connector.Connector, runner Runner) *Orchestrator {
	t.Helper()

	orch, err := New(cfg, Dependencies{Connector: conn, Runner: runner})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return orch
}

func receiveClaimAttempt(t *testing.T, attempts <-chan string) string {
	t.Helper()

	select {
	case login := <-attempts:
		return login
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for claim attempt")
		return ""
	}
}

func receiveClaimRun(t *testing.T, started <-chan RunRequest) RunRequest {
	t.Helper()

	select {
	case request := <-started:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner start")
		return RunRequest{}
	}
}

func receiveOptionalClaimRun(started <-chan RunRequest) (RunRequest, bool) {
	select {
	case request := <-started:
		return request, true
	case <-time.After(50 * time.Millisecond):
		return RunRequest{}, false
	}
}

func receiveClaimCancellation(t *testing.T, canceled <-chan struct{}) {
	t.Helper()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}
}

var _ connector.Connector = claimTestConnector{}
var _ connector.InstanceIdentifier = claimTestConnector{}
var _ Runner = (*claimBlockingRunner)(nil)
