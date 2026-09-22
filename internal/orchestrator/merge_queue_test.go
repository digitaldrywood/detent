package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestDelegateNativeMergeQueueIssuesEnqueuesGreenTrainWithoutWorkerDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	issues := make([]connector.Issue, 3)
	for index := range issues {
		number := 101 + index
		issues[index] = autoPromoteTickIssue(fmt.Sprintf("issue-%d", number), []string{"bug"}, &connector.PullRequest{
			NodeID:         fmt.Sprintf("PR_%d", number),
			Number:         number,
			URL:            fmt.Sprintf("https://github.test/digitaldrywood/detent/pull/%d", number),
			BranchName:     fmt.Sprintf("detent/issue-%d", number),
			BaseRef:        "main",
			State:          "OPEN",
			MergeableState: "clean",
			CIStatus:       "success",
			HeadSHA:        fmt.Sprintf("head-%d", number),
		})
		if index == 0 {
			issues[index].PullRequest.CIStatus = "pending"
			issues[index].PullRequest.Checks = []connector.PullRequestCheck{{Name: "Test", Status: "completed", Conclusion: "skipped"}}
			issues[index].PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Test", Status: "pending"}}
		}
		issues[index].State = "Merging"
		issues[index].Identifier = fmt.Sprintf("digitaldrywood/detent#%d", number)
		issues[index].PRRepository = "digitaldrywood/detent"
	}

	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
	}
	cfg := nativeMergeQueueTestConfig(Config{
		MergeFastPathEnabled: true,
		MaxConcurrentAgents:  1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, issues, now)

	if got, want := tracker.enqueued, []string{"issue-101", "issue-102", "issue-103"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued issue ids = %#v, want %#v", got, want)
	}
	if tracker.inspections != len(issues) {
		t.Fatalf("merge queue inspections = %d, want %d", tracker.inspections, len(issues))
	}
	if len(queued) != len(issues) {
		t.Fatalf("queued issues len = %d, want %d", len(queued), len(issues))
	}
	for _, issue := range queued {
		if issue.PullRequest == nil || issue.PullRequest.MergeQueueEntry == nil {
			t.Fatalf("issue %q merge queue entry missing", issue.ID)
		}
	}
	if candidates := orch.mergeWorkerDispatchCandidates(&state, queued, now); len(candidates) != 0 {
		t.Fatalf("merge worker candidates = %#v, want native queue entries excluded", candidates)
	}
}

func TestDelegateNativeMergeQueueIssuesSerializesOnlyRunningHeads(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 4, 20, 0, 0, time.UTC)
	agedAt := now.Add(-3 * time.Hour)
	aged := nativeMergeQueueTestIssue(1748, "pending")
	aged.ID = "issue-aged-native-head"
	aged.StageUpdatedAt = &agedAt
	recent := nativeMergeQueueTestIssue(1749, "success")
	recent.ID = "issue-recent-native-head"
	recent.Identifier = "digitaldrywood/pyroapex#1749"
	recent.PRRepository = "digitaldrywood/pyroapex"
	recent.PullRequest.URL = "https://github.test/digitaldrywood/pyroapex/pull/1749"
	recentAt := now.Add(-time.Minute)
	recent.StageUpdatedAt = &recentAt
	cfg := nativeMergeQueueTestConfig(Config{
		MergeFastPathEnabled: true,
		MaxConcurrentAgents:  1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		ActiveStates:   []string{"Merging"},
		TerminalStates: []string{"Done"},
	})

	tests := []struct {
		name           string
		setup          func(*State)
		sameRepository bool
		wantEnqueued   int
	}{
		{
			name:         "running aged head allows other repository",
			wantEnqueued: 1,
			setup: func(state *State) {
				state.Running[aged.ID] = Running{Issue: aged, StartedAt: now.Add(-time.Minute)}
			},
		},
		{
			name:         "retrying aged head allows other repository",
			wantEnqueued: 1,
			setup: func(state *State) {
				state.Retry[aged.ID] = Retry{Issue: aged, DueAt: now.Add(time.Minute)}
			},
		},
		{
			name:           "running aged head protects same repository",
			sameRepository: true,
			setup: func(state *State) {
				state.Running[aged.ID] = Running{Issue: aged, StartedAt: now.Add(-time.Minute)}
			},
		},
		{
			name:           "retrying aged head allows same repository",
			wantEnqueued:   1,
			sameRepository: true,
			setup: func(state *State) {
				state.Retry[aged.ID] = Retry{Issue: aged, DueAt: now.Add(time.Minute)}
			},
		},
		{
			name:           "CI wait allows same repository",
			wantEnqueued:   1,
			sameRepository: true,
			setup:          func(state *State) { reserveMergeCandidate(state, aged, now) },
		},
		{
			name:         "CI reservation allows other repository",
			wantEnqueued: 1,
			setup:        func(state *State) { reserveMergeCandidate(state, aged, now) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &nativeMergeQueueConnector{
				autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
					autoPromoteTickConnector: &autoPromoteTickConnector{},
				},
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			tt.setup(&state)
			recent := cloneIssue(recent)
			if tt.sameRepository {
				recent.PRRepository = aged.PRRepository
			}

			queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{aged, recent}, now)

			if len(tracker.enqueued) != tt.wantEnqueued || tracker.inspections < tt.wantEnqueued {
				t.Fatalf("native queue activity = enqueued %#v, inspections %d; want %d", tracker.enqueued, tracker.inspections, tt.wantEnqueued)
			}
			for _, issue := range queued {
				wantEntry := issue.ID == recent.ID && tt.wantEnqueued == 1
				if gotEntry := issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil; gotEntry != wantEntry {
					t.Fatalf("issue %q has entry = %t, want %t", issue.ID, gotEntry, wantEntry)
				}
			}
		})
	}
}

func TestTickDelegatesNativeMergeQueueTrainWithoutAgentDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	issues := []connector.Issue{
		nativeMergeQueueTestIssue(111, "success"),
		nativeMergeQueueTestIssue(112, "success"),
		nativeMergeQueueTestIssue(113, "success"),
	}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{
				stateIssues:        issues,
				candidateIssues:    issues,
				candidateIssuesSet: true,
			},
		},
	}
	cfg := nativeMergeQueueTestConfig(Config{
		PollInterval:         time.Minute,
		MergeFastPathEnabled: true,
		MaxConcurrentAgents:  1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	var logs strings.Builder
	runner := newWorkerHostRunner()
	orch := &Orchestrator{
		cfg:        cfg,
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)
	orch.tick(context.Background(), &state, now.Add(10*time.Second))

	if got, want := tracker.enqueued, []string{"issue-111", "issue-112", "issue-113"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued issue ids = %#v, want %#v", got, want)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected merge worker dispatch for %q", request.Issue.ID)
	default:
	}
	if strings.Contains(logs.String(), "merge_worker_attempt") {
		t.Fatal(logs.String())
	}
	if got := strings.Count(logs.String(), "msg=merge_queue_entered "); got != len(issues) {
		t.Fatalf("queue entered events=%d want %d: %s", got, len(issues), logs.String())
	}
	queued := 0
	for _, issue := range state.BoardIssues {
		if issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil {
			queued++
		}
	}
	if queued != len(issues) {
		t.Fatalf("board native queue entries = %d, want %d", queued, len(issues))
	}
}

func TestDelegateNativeMergeQueueIssuesCachesQueueEntries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		elapsed   time.Duration
		newHead   bool
		wantCalls int
	}{
		{name: "cached", elapsed: 10 * time.Second, wantCalls: 1},
		{name: "refresh", elapsed: nativeMergeQueueEntryRefresh, wantCalls: 2},
		{name: "new head", elapsed: 10 * time.Second, newHead: true, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
			issue := nativeMergeQueueTestIssue(201, "success")
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			first := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			tracker.entries = map[string]connector.PullRequestMergeQueueEntry{issue.ID: *first[0].PullRequest.MergeQueueEntry}
			if tt.newHead {
				first[0].PullRequest.HeadSHA = "replacement"
			}
			second := orch.delegateNativeMergeQueueIssues(t.Context(), &state, first, now.Add(tt.elapsed))
			if tracker.inspections != tt.wantCalls || len(tracker.reviewThreadHydrations) != tt.wantCalls || len(tracker.enqueued) != 1 {
				t.Fatalf("queue calls: inspections=%d hydrations=%d enqueues=%d, want %d, %d, 1", tracker.inspections, len(tracker.reviewThreadHydrations), len(tracker.enqueued), tt.wantCalls, tt.wantCalls)
			}
			if second[0].PullRequest.MergeQueueEntry == nil {
				t.Fatal("cached queue entry missing")
			}
		})
	}
}

func TestDelegateNativeMergeQueueIssuesWaitsForMissingEntryOutcome(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(202, "success")
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
	}
	cfg := nativeMergeQueueTestConfig(Config{
		MergeFastPathEnabled: true,
		ActiveStates:         []string{"Merging"},
		TerminalStates:       []string{"Done"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	first := orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{issue}, now)
	second := orch.delegateNativeMergeQueueIssues(context.Background(), &state, first, now.Add(nativeMergeQueueEntryRefresh))

	if tracker.inspections != 2 || len(tracker.enqueued) != 1 {
		t.Fatalf("queue calls = %d inspections and %d enqueues, want two inspections and one enqueue after entry disappears", tracker.inspections, len(tracker.enqueued))
	}
	if second[0].PullRequest == nil || second[0].PullRequest.MergeQueueEntry == nil || second[0].PullRequest.MergeQueueEntry.State == "MISSING" {
		t.Fatalf("re-enqueued queue entry = %#v, want active replacement", second[0].PullRequest)
	}
}

func TestDelegateNativeMergeQueueIssuesRefreshesQueueOwnership(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                    string
		available, entryPresent bool
		wantWorkers             int
	}{
		{name: "queue and entry gone", wantWorkers: 1},
		{name: "queue remains", available: true},
		{name: "provider entry remains", entryPresent: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
			issues := []connector.Issue{nativeMergeQueueTestIssue(203, "success"), nativeMergeQueueTestIssue(204, "success")}
			available := true
			tracker := &nativeMergeQueueConnector{
				autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}},
				available:                     &available,
			}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, MaxConcurrentAgentsByState: map[string]int{"Merging": 1}, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			first := orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, now)
			available = tt.available
			if tt.entryPresent {
				tracker.entries = map[string]connector.PullRequestMergeQueueEntry{}
				for _, issue := range first {
					tracker.entries[issue.ID] = *issue.PullRequest.MergeQueueEntry
				}
			}
			second := orch.delegateNativeMergeQueueIssues(t.Context(), &state, first, now.Add(nativeMergeQueueEntryRefresh))
			if len(tracker.enqueued) != 2 || len(tracker.dequeued) != 0 {
				t.Fatalf("queue mutations: enqueue=%v dequeue=%v", tracker.enqueued, tracker.dequeued)
			}
			for _, issue := range second {
				if got := nativeMergeQueueOwnsIssue(&state, issue, orch.cfg); got != (tt.wantWorkers == 0) {
					t.Fatalf("%s ownership=%t", issue.ID, got)
				}
			}
			candidates := orch.mergeWorkerDispatchCandidates(&state, second, now.Add(nativeMergeQueueEntryRefresh))
			if len(candidates) != tt.wantWorkers {
				t.Fatalf("worker candidates=%d want %d", len(candidates), tt.wantWorkers)
			}
		})
	}
}

func TestDelegateNativeMergeQueueIssuesRecoversExistingEntriesAfterRestart(t *testing.T) {
	t.Parallel()

	issues := []connector.Issue{
		nativeMergeQueueTestIssue(211, "success"),
		nativeMergeQueueTestIssue(212, "success"),
	}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
		entries: map[string]connector.PullRequestMergeQueueEntry{
			"issue-211": {ID: "MQE_211", State: "AWAITING_CHECKS", Position: 1, Depth: 2},
			"issue-212": {ID: "MQE_212", State: "QUEUED", Position: 2, Depth: 2},
		},
	}
	cfg := nativeMergeQueueTestConfig(Config{
		MergeFastPathEnabled: true,
		ActiveStates:         []string{"Merging"},
		TerminalStates:       []string{"Done"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, issues, time.Now())

	if len(tracker.enqueued) != 0 {
		t.Fatalf("enqueued = %#v, want existing entries observed without mutation", tracker.enqueued)
	}
	if candidates := orch.mergeWorkerDispatchCandidates(&state, queued, time.Now()); len(candidates) != 0 {
		t.Fatalf("merge worker candidates = %#v, want recovered queue entries excluded", candidates)
	}
}

func TestDelegateNativeMergeQueueIssuesFallsBackWithoutNativeQueue(t *testing.T) {
	t.Parallel()

	available := false
	issue := nativeMergeQueueTestIssue(301, "success")
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
		available: &available,
	}
	cfg := nativeMergeQueueTestConfig(Config{
		MergeFastPathEnabled: true,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		ActiveStates:   []string{"Merging"},
		TerminalStates: []string{"Done"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{issue}, time.Now())

	if len(tracker.enqueued) != 0 {
		t.Fatalf("enqueued = %#v, want no native enqueue", tracker.enqueued)
	}
	candidates := orch.mergeWorkerDispatchCandidates(&state, queued, time.Now())
	if len(candidates) != 1 || candidates[0].ID != issue.ID {
		t.Fatalf("merge worker candidates = %#v, want fallback issue %q", candidates, issue.ID)
	}
}

func TestNativeMergeQueueCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*connector.Issue)
		want   bool
	}{
		{name: "green current head", want: true},
		{name: "missing checked head", mutate: func(issue *connector.Issue) { issue.PullRequest.HeadSHA = "" }},
		{name: "pending CI", mutate: func(issue *connector.Issue) { issue.PullRequest.CIStatus = "pending" }},
		{name: "failed CI", mutate: func(issue *connector.Issue) { issue.PullRequest.CIStatus = "failure" }},
		{name: "draft", mutate: func(issue *connector.Issue) { issue.PullRequest.Draft = true }},
		{name: "closed", mutate: func(issue *connector.Issue) { issue.PullRequest.State = "CLOSED" }},
		{name: "missing pull request", mutate: func(issue *connector.Issue) { issue.PullRequest = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := nativeMergeQueueTestIssue(401, "success")
			if tt.mutate != nil {
				tt.mutate(&issue)
			}
			if got := nativeMergeQueueCandidate(issue, nativeMergeQueueTestConfig(Config{})); got != tt.want {
				t.Fatalf("nativeMergeQueueCandidate() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestNativeMergeQueueCandidateRejectsNonAtomicSecurityAuditGate(t *testing.T) {
	t.Parallel()

	issue := nativeMergeQueueTestIssue(402, "success")
	cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Gate: gate.Config{
		Kind:          gate.KindArtifact,
		SecurityAudit: gate.SecurityAuditConfig{Enabled: true},
	}}})
	if nativeMergeQueueCandidate(issue, cfg) {
		t.Fatal("nativeMergeQueueCandidate() = true, want programmatic exact-head merge")
	}
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

type nativeMergeQueueConnector struct {
	*autoPromoteTickMergeConnector
	available       *bool
	inspectErr      error
	enqueueErr      error
	hydrationErr    error
	hydratedThreads *[]connector.PullRequestReviewThread
	inspectedHead   string
	admissionLimit  *int
	removedHeads    map[string]string
	removalReason   string
	removedAt       *time.Time
	dequeueErr      error
	statusErr       error
	inspections     int
	entries         map[string]connector.PullRequestMergeQueueEntry
	enqueuedAt      *time.Time
	enqueued        []string
	dequeued        []connector.PullRequestMergeQueueEntry
}

func (c *nativeMergeQueueConnector) FetchIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	return c.autoPromoteTickConnector.FetchIssuesByStates(ctx, states)
}

func (c *nativeMergeQueueConnector) InspectPullRequestMergeQueue(_ context.Context, issue connector.Issue) (connector.PullRequestMergeQueueStatus, error) {
	c.inspections++
	if c.inspectErr != nil {
		return connector.PullRequestMergeQueueStatus{}, c.inspectErr
	}
	available := true
	if c.available != nil {
		available = *c.available
	}
	limit := 100
	if c.admissionLimit != nil {
		limit = *c.admissionLimit
	}
	status := connector.PullRequestMergeQueueStatus{Available: available, AdmissionLimit: limit, Depth: len(c.enqueued)}
	status.RemovedHeadSHA, status.RemovalObserved = c.removedHeads[issue.ID]
	status.RemovalReason = c.removalReason
	status.RemovedAt = c.removedAt
	if issue.PullRequest != nil {
		status.HeadSHA = issue.PullRequest.HeadSHA
	}
	if c.inspectedHead != "" {
		status.HeadSHA = c.inspectedHead
	}
	if entry, ok := c.entries[issue.ID]; ok {
		status.Entry = &entry
	}
	return status, nil
}

func (c *nativeMergeQueueConnector) HydratePullRequestReviewThreads(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	if c.hydrationErr != nil {
		return issue, c.hydrationErr
	}
	hydrated, err := c.autoPromoteTickConnector.HydratePullRequestReviewThreads(ctx, issue)
	if c.hydratedThreads != nil {
		hydrated.PullRequest.UnresolvedReviewThreads = *c.hydratedThreads
	}
	return hydrated, err
}

func (c *nativeMergeQueueConnector) EnqueuePullRequest(_ context.Context, issue connector.Issue) (connector.PullRequestMergeQueueEntry, error) {
	c.enqueued = append(c.enqueued, issue.ID)
	if c.enqueueErr != nil {
		return connector.PullRequestMergeQueueEntry{}, c.enqueueErr
	}
	enqueuedAt := c.enqueuedAt
	if enqueuedAt == nil {
		enqueuedAt = timePointer(time.Now())
	}
	return connector.PullRequestMergeQueueEntry{
		ID:                          "MQE_" + issue.ID,
		State:                       "QUEUED",
		Position:                    len(c.enqueued),
		Depth:                       len(c.enqueued),
		EstimatedTimeToMergeSeconds: int64(len(c.enqueued)) * 60,
		EnqueuedAt:                  enqueuedAt,
		URL:                         "https://github.test/digitaldrywood/detent/queue/main",
	}, nil
}

func (c *nativeMergeQueueConnector) DequeuePullRequest(_ context.Context, entry connector.PullRequestMergeQueueEntry) error {
	c.dequeued = append(c.dequeued, entry)
	return c.dequeueErr
}

func TestNativeMergeQueueIntegrationContinuity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		changeHead  bool
		advanceBase bool
		restart     bool
		cancel      bool
	}{
		{name: "passing heads retain integration"},
		{name: "advancing main retains provider integration", advanceBase: true},
		{name: "unreviewed head cannot reuse cache", changeHead: true},
		{name: "restart observes provider entries", restart: true},
		{name: "cancellation stops admission", cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			issues := []connector.Issue{nativeMergeQueueTestIssue(501, "success"), nativeMergeQueueTestIssue(502, "success"), nativeMergeQueueTestIssue(503, "failure")}
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			queued := orch.delegateNativeMergeQueueIssues(ctx, &state, issues, now)
			if tt.cancel {
				if tracker.inspections != 0 || len(tracker.enqueued) != 0 {
					t.Fatal("cancelled admission contacted provider")
				}
				if candidates := orch.mergeWorkerDispatchCandidates(&state, queued, now); len(candidates) != 0 {
					t.Fatalf("cancelled admission left %d worker candidates", len(candidates))
				}
				return
			}
			if len(tracker.enqueued) != 2 {
				t.Fatalf("enqueued = %v, want two passing candidates", tracker.enqueued)
			}
			tracker.entries = map[string]connector.PullRequestMergeQueueEntry{}
			for _, issue := range queued {
				if issue.PullRequest.MergeQueueEntry != nil {
					tracker.entries[issue.ID] = *issue.PullRequest.MergeQueueEntry
				}
			}
			if tt.advanceBase {
				issues[0].PullRequest.BaseSHA = "new-main"
				issues[1].PullRequest.BaseSHA = "new-main"
			}
			if tt.changeHead {
				issues[0].PullRequest.HeadSHA = "unreviewed"
				tracker.inspectedHead = "another-head"
			}
			if tt.restart {
				state = newState(cfg)
			}
			orch.delegateNativeMergeQueueIssues(ctx, &state, issues, now.Add(time.Second))
			wantInspections := 4
			if tt.changeHead {
				wantInspections++
			}
			if tt.restart {
				wantInspections += 2
			}
			if tracker.inspections != wantInspections || len(tracker.enqueued) != 2 {
				t.Fatalf("inspections = %d, enqueue = %v", tracker.inspections, tracker.enqueued)
			}
			if tt.changeHead {
				if _, deferred := state.nativeMergeQueueDeferred[issues[0].ID]; !deferred {
					t.Fatal("changed head was not deferred")
				}
			}
		})
	}
}

func TestNativeMergeQueueUrgentAdmissionPreservesAging(t *testing.T) {
	t.Parallel()
	for _, aged := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary_aged_%t", aged), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			ordinary := nativeMergeQueueTestIssue(601, "success")
			urgent := nativeMergeQueueTestIssue(602, "success")
			urgent.Labels = []string{"hotfix"}
			recent := now.Add(-time.Minute)
			ordinary.StageUpdatedAt = &recent
			urgent.StageUpdatedAt = &recent
			if aged {
				old := now.Add(-3 * time.Hour)
				ordinary.StageUpdatedAt = &old
			}
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, MergeFairnessAge: time.Hour, DispatchPriorityByLabel: []string{"hotfix"}, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{ordinary, urgent}, now)
			want := []string{urgent.ID, ordinary.ID}
			if aged {
				want = []string{ordinary.ID, urgent.ID}
			}
			if !reflect.DeepEqual(tracker.enqueued, want) {
				t.Fatalf("enqueued = %v, want %v", tracker.enqueued, want)
			}
		})
	}
}

func TestNativeMergeQueueBoundsAdmissionWithoutWorkerFallback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	limit := 2
	tracker := &nativeMergeQueueConnector{admissionLimit: &limit, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
	cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, DispatchPriorityByLabel: []string{"hotfix"}, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	issues := []connector.Issue{nativeMergeQueueTestIssue(701, "success"), nativeMergeQueueTestIssue(702, "success"), nativeMergeQueueTestIssue(703, "success")}
	queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, issues, now)
	if len(tracker.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want bounded pair", tracker.enqueued)
	}
	if _, deferred := state.nativeMergeQueueDeferred[issues[2].ID]; !deferred {
		t.Fatal("full window candidate not deferred")
	}
	if candidates := orch.mergeWorkerDispatchCandidates(&state, queued, now); len(candidates) != 0 {
		t.Fatalf("worker candidates = %v, want no integration fallback", candidates)
	}
	urgent := nativeMergeQueueTestIssue(704, "success")
	urgent.Labels = []string{"hotfix"}
	tracker.enqueued = nil
	orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{issues[2], urgent}, now.Add(time.Minute))
	if want := []string{urgent.ID, issues[2].ID}; !reflect.DeepEqual(tracker.enqueued, want) {
		t.Fatalf("next window = %v, want %v", tracker.enqueued, want)
	}
}

func TestNativeMergeQueueRemovalSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		removedHead     string
		wantReenqueue   bool
		manualReenqueue bool
	}{
		{name: "failed integrated head", removedHead: "head-802"},
		{name: "cancelled entry with missing identity"},
		{name: "explicit provider reenqueue", removedHead: "head-802", manualReenqueue: true},
		{name: "new checked head after repair", removedHead: "old-head", wantReenqueue: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := []connector.Issue{nativeMergeQueueTestIssue(801, "success"), nativeMergeQueueTestIssue(802, "success"), nativeMergeQueueTestIssue(803, "success")}
			tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issues[1].ID: tt.removedHead}, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			if tt.manualReenqueue {
				tracker.entries = map[string]connector.PullRequestMergeQueueEntry{issues[1].ID: {ID: "operator-entry", State: "QUEUED"}}
			}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			for range 2 {
				state := newState(cfg)
				tracker.enqueued = nil
				queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, issues, time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
				if !tt.manualReenqueue && !tt.wantReenqueue && queued[1].State != "Merging" {
					t.Fatalf("removed queue issue state = %q, want Merging", queued[1].State)
				}
				if tt.manualReenqueue && (queued[1].PullRequest.MergeQueueEntry == nil || queued[1].PullRequest.MergeQueueEntry.ID != "operator-entry") {
					t.Fatal("explicit provider entry not observed")
				}
				want := []string{issues[0].ID, issues[2].ID}
				if tt.wantReenqueue {
					want = []string{issues[0].ID, issues[1].ID, issues[2].ID}
				}
				if !reflect.DeepEqual(tracker.enqueued, want) {
					t.Fatalf("enqueued = %v, want %v", tracker.enqueued, want)
				}
			}
		})
	}
}

func TestNativeMergeQueueUnknownAdmissionLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()
			issue := nativeMergeQueueTestIssue(901, "success")
			tracker := &nativeMergeQueueConnector{admissionLimit: &limit, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			queued := orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{issue}, now)
			if len(tracker.enqueued) != 0 {
				t.Fatalf("enqueued = %v, want unknown window deferred", tracker.enqueued)
			}
			if candidates := orch.mergeWorkerDispatchCandidates(&state, queued, now); len(candidates) != 0 {
				t.Fatalf("candidates = %v, want no worker fallback", candidates)
			}
		})
	}
}

func TestNativeMergeQueueRejectionRecordsProviderReason(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"Required check build failed", ""} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			issue := nativeMergeQueueTestIssue(990, "success")
			issue.PullRequest.CIDurationSeconds = 1320
			issue.PullRequest.BaseRef = "main"
			tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: reason, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			recorder := &workflowMetricsRecorderSpy{}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: recorder}
			state := newState(cfg)
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if got[0].State != "Merging" || len(tracker.enqueued) != 0 {
				t.Fatalf("issues=%+v enqueue=%v", got, tracker.enqueued)
			}
			if len(recorder.events) != 0 || len(tracker.updates) != 0 {
				t.Fatalf("queue removal emitted lane transitions: %+v", recorder.events)
			}
			removals := state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]
			if len(removals) != 1 || removals[0] == "" || (reason != "" && !strings.Contains(removals[0], reason)) {
				t.Fatalf("provider reason missing: %v", removals)
			}
			if len(orch.mergeWorkerDispatchCandidates(&state, got, now)) != 0 {
				t.Fatal("rejection entered merge worker CI loop")
			}
		})
	}
}

func TestNativeQueueRepositoryOwnsDispatch(t *testing.T) {
	t.Parallel()
	for _, available := range []bool{false, true} {
		t.Run(fmt.Sprintf("available=%t", available), func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2459, "success")
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, MaxConcurrentAgents: 1, ActiveStates: []string{"Merging"}})
			orch := &Orchestrator{cfg: cfg}
			state := newState(cfg)
			now := time.Now()
			state.nativeMergeQueueRepos[nativeMergeQueueRepositoryKey(issue)] = nativeMergeQueueRepository{Available: available, CheckedAt: now}
			candidates := orch.mergeWorkerDispatchCandidates(&state, []connector.Issue{issue}, now)
			if available && len(candidates) != 0 {
				t.Fatalf("queue available dispatched %d workers", len(candidates))
			}
			if !available && len(candidates) != 1 {
				t.Fatalf("queue unavailable dispatched %d workers", len(candidates))
			}
		})
	}
}

func TestNativeMergeQueueWorkerHandoff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		known      bool
		workerErr  error
		inspectErr error
	}{
		{name: "known queue", known: true},
		{name: "known queue worker failure", known: true, workerErr: errors.New("obsolete fallback failed")},
		{name: "stale policy 405"},
		{name: "405 inspection failure", inspectErr: errors.New("provider unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2459, "success")
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done"}})
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, err: connector.ErrPullRequestMergeQueueRequired}, inspectErr: tt.inspectErr}
			var logs strings.Builder
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			state := newState(cfg)
			now := time.Now()
			state.nativeMergeQueueRepos[nativeMergeQueueRepositoryKey(issue)] = nativeMergeQueueRepository{Available: tt.known, CheckedAt: now}
			running := Running{Issue: issue, Attempt: maxMergeWorkerRunnerFailures, StartedAt: now.Add(-time.Minute)}
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted}}
			state.Running[issue.ID] = running
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now}
			event.Err = tt.workerErr
			orch.handleRunResult(t.Context(), &state, event)
			wantMerges := 1
			if tt.known {
				wantMerges = 0
			}
			if len(tracker.merges) != wantMerges {
				t.Fatalf("merges=%d want %d", len(tracker.merges), wantMerges)
			}
			if len(state.Retry) != 0 || len(tracker.updates) != 0 {
				t.Fatalf("retry or lane change: %v %v", state.Retry, tracker.updates)
			}
			if tt.inspectErr == nil && len(tracker.enqueued) != 1 {
				t.Fatalf("enqueued=%v", tracker.enqueued)
			}
			if tracker.inspections != 1 {
				t.Fatalf("inspections=%d", tracker.inspections)
			}
			if !nativeMergeQueueOwnsIssue(&state, issue, orch.cfg) {
				t.Fatal("lost queue ownership")
			}
			if strings.Contains(logs.String(), "merge_worker_retry_exhausted") {
				t.Fatal(logs.String())
			}
		})
	}
}

func TestNativeMergeQueueOutcomes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, state, ci, want string }{
		{"queued checks failing", "OPEN", "failure", ""},
		{"queue merged", "MERGED", "success", "Done"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2460, tt.ci)
			issue.PullRequest.State = tt.state
			issue.PullRequest.MergeQueueEntry = &connector.PullRequestMergeQueueEntry{ID: "entry"}
			cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done"}})
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
			if tt.want == "" {
				if len(tracker.updates) != 0 {
					t.Fatalf("updates=%v", tracker.updates)
				}
			} else if len(tracker.updates) != 1 || tracker.updates[0].state != tt.want {
				t.Fatalf("updates=%v want %s", tracker.updates, tt.want)
			}
			if len(tracker.dequeued) != 0 || len(tracker.merges) != 0 {
				t.Fatal("programmatic queue mutation")
			}
		})
	}
}

func TestNativeMergeQueueRetainsEntryAcrossConfigurationChanges(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{"fast path disabled", func(c *Config) { c.MergeFastPathEnabled = false }},
		{"review gate enabled", func(c *Config) { c.AutoPromote.Gate.Kind = gate.KindCommand }},
		{"audit enabled", func(c *Config) { c.AutoPromote.Gate.SecurityAudit.Enabled = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2461, "success")
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}})
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			now := time.Now()
			queued := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			tracker.entries = map[string]connector.PullRequestMergeQueueEntry{issue.ID: *queued[0].PullRequest.MergeQueueEntry}
			tt.change(&orch.cfg)
			queued = orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(nativeMergeQueueEntryRefresh))
			if len(tracker.dequeued) != 0 || len(tracker.enqueued) != 1 || queued[0].PullRequest.MergeQueueEntry == nil {
				t.Fatal("configuration changed provider entry")
			}
			if got := orch.mergeWorkerDispatchCandidates(&state, queued, now); len(got) != 0 {
				t.Fatal("configuration enabled worker fallback")
			}
		})
	}
}

func TestNativeMergeQueueUnadmittedFailureReconciles(t *testing.T) {
	t.Parallel()
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2462, "failure")
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			now := time.Now()
			state.nativeMergeQueueRepos[nativeMergeQueueRepositoryKey(issue)] = nativeMergeQueueRepository{Available: true, CheckedAt: now}
			if queued {
				tracker.entries = map[string]connector.PullRequestMergeQueueEntry{issue.ID: {ID: "entry"}}
			}
			issues := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, issues, now.Add(time.Second))
			if queued {
				if len(tracker.updates) != 0 {
					t.Fatalf("queued PR transitioned: %v", tracker.updates)
				}
			} else if len(tracker.updates) != 1 || tracker.updates[0].state != "Rework" {
				t.Fatalf("unadmitted failure updates=%v, want Rework", tracker.updates)
			}
			if len(tracker.enqueued) != 0 || len(tracker.merges) != 0 || len(tracker.dequeued) != 0 {
				t.Fatal("unexpected queue or merge mutation")
			}
		})
	}
}

func TestNativeMergeQueueRemovalOrdering(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 3, 50, 0, 0, time.UTC)
	for _, tt := range []struct {
		name         string
		offset       time.Duration
		missingTime  bool
		sameHead     bool
		localEnqueue bool
		wantRemoval  bool
	}{
		{name: "newer removal with non-head beforeCommit", offset: time.Minute, wantRemoval: true},
		{name: "older removal with non-head beforeCommit", offset: -time.Minute},
		{name: "older removal even with matching head", offset: -time.Minute, sameHead: true},
		{name: "equal timestamp", sameHead: true},
		{name: "missing removal timestamp", missingTime: true, sameHead: true},
		{name: "local enqueue timestamp", offset: time.Minute, localEnqueue: true, wantRemoval: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := nativeMergeQueueTestIssue(2474, "success")
			removedHead := "merge-group-before-commit"
			if tt.sameHead {
				removedHead = issue.PullRequest.HeadSHA
			}
			tracker := &nativeMergeQueueConnector{
				removedHeads:                  map[string]string{issue.ID: removedHead},
				removalReason:                 "failed_checks",
				removedAt:                     timePointer(now.Add(tt.offset)),
				autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}},
			}
			if tt.missingTime {
				tracker.removedAt = nil
			}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			recorder := &workflowMetricsRecorderSpy{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: recorder}
			state := newState(cfg)
			entry := connector.PullRequestMergeQueueEntry{ID: "cached-entry", State: "QUEUED"}
			if !tt.localEnqueue {
				entry.EnqueuedAt = timePointer(now)
			}
			cacheNativeMergeQueueEntry(&state, issue.ID, entry, now)
			got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(3*time.Minute))
			if len(tracker.enqueued) != 0 {
				t.Fatalf("unexpected enqueue: %v", tracker.enqueued)
			}
			_, cached := state.nativeMergeQueueEntries[issue.ID]
			if tt.wantRemoval {
				if got[0].State != "Merging" || cached || got[0].PullRequest.MergeQueueEntry != nil {
					t.Fatalf("removal not applied: issue=%+v cached=%v", got[0], cached)
				}
				if len(recorder.events) != 0 || len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]) != 1 {
					t.Fatalf("provider reason missing: %+v", recorder.events)
				}
			} else if got[0].State != "Merging" || !cached || got[0].PullRequest.MergeQueueEntry == nil || got[0].PullRequest.MergeQueueEntry.ID != entry.ID {
				t.Fatalf("cached entry not retained: issue=%+v cached=%v", got[0], cached)
			}
		})
	}
}

func TestNativeMergeQueueAttemptBudget(t *testing.T) {
	t.Parallel()
	issue := nativeMergeQueueTestIssue(950, "success")
	first := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: "Required check build failed", removedAt: &first, enqueuedAt: timePointer(first.Add(2 * time.Minute)), autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
	cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done"}})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, first.Add(time.Minute))
	if got[0].State != "Merging" || len(tracker.comments) != 0 {
		t.Fatalf("first removal: state=%q comments=%d", got[0].State, len(tracker.comments))
	}
	got = orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, first.Add(2*time.Minute))
	if got[0].State != "Merging" || len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]) != 1 {
		t.Fatalf("repeated observation of one removal: state=%q removals=%v", got[0].State, state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)])
	}

	tracker.removalReason, tracker.removedAt = "Merge conflict", &second
	got = orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, second.Add(time.Minute))
	if got[0].State != "Rework" {
		t.Fatalf("second removal: state=%q, want %s", got[0].State, autoPromoteSourceState)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one budget comment", tracker.comments)
	}
	for _, want := range []string{string(AutoPromoteReasonMergeRevocationLimit), "Required check build failed", "Merge conflict", "limit: 2", issue.PullRequest.URL} {
		if !strings.Contains(tracker.comments[0].body, want) {
			t.Fatalf("comment %q missing %q", tracker.comments[0].body, want)
		}
	}
	if len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]) != 2 {
		t.Fatal("exhausted head budget was not retained after routing")
	}
	if len(tracker.enqueued) != 1 {
		t.Fatalf("enqueued = %v, want one retry", tracker.enqueued)
	}
}

func TestNativeMergeQueueAttemptBudgetResetsWhenIssueLeaves(t *testing.T) {
	t.Parallel()
	issue := nativeMergeQueueTestIssue(951, "success")
	removed := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: "Required check build failed", removedAt: &removed, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
	cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done"}})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	if got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, removed.Add(time.Minute)); got[0].State != "Merging" {
		t.Fatalf("state = %q, want Merging", got[0].State)
	}
	orch.delegateNativeMergeQueueIssues(t.Context(), &state, nil, removed.Add(2*time.Minute))
	if len(state.nativeMergeQueueRemovals) != 0 {
		t.Fatalf("removals = %v, want pruned once the issue merged", state.nativeMergeQueueRemovals)
	}
}

func TestNativeMergeQueueRepeatedRemovalRetries(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached_%t", cached), func(t *testing.T) {
			t.Parallel()
			issue := nativeMergeQueueTestIssue(2586, "success")
			removed := time.Date(2026, 9, 14, 15, 44, 41, 0, time.UTC)
			tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: "failed_checks", removedAt: &removed, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			if cached {
				cacheNativeMergeQueueEntry(&state, issue.ID, connector.PullRequestMergeQueueEntry{ID: "old-entry"}, removed.Add(-10*time.Minute))
			}
			for pass := range 3 {
				if pass == 1 {
					tracker.removalReason = "updated provider description"
				}
				got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, removed.Add(time.Duration(pass+1)*3*time.Minute))
				if got[0].State != "Merging" || len(tracker.updates) != 0 {
					t.Fatalf("pass %d: state=%s updates=%v", pass, got[0].State, tracker.updates)
				}
				want := 1
				if pass == 0 {
					want = 0
				}
				if len(tracker.enqueued) != want {
					t.Fatalf("pass %d: enqueued=%v, want %d", pass, tracker.enqueued, want)
				}
				if len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]) != 1 {
					t.Fatalf("removals=%v", state.nativeMergeQueueRemovals)
				}
			}
		})
	}
}

func TestNativeMergeQueueBudgetSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, restartBeforeRetry := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart_before_retry_%t", restartBeforeRetry), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "queue.db")
			backend, err := store.Open(t.Context(), store.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			})
			issue := nativeMergeQueueTestIssue(2621, "success")
			first := time.Date(2026, 9, 14, 15, 44, 41, 0, time.UTC)
			tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: "failed_checks", removedAt: &first, enqueuedAt: timePointer(first.Add(2 * time.Minute)), autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: backend}
			state := newState(cfg)
			restart := func() {
				if err := backend.Close(); err != nil {
					t.Fatal(err)
				}
				backend, err = store.Open(t.Context(), store.Config{Path: path})
				if err != nil {
					t.Fatal(err)
				}
				orch = &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: backend}
				state = newState(cfg)
			}
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, first.Add(time.Minute))
			if restartBeforeRetry {
				restart()
			}
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, first.Add(2*time.Minute))
			if len(tracker.enqueued) != 1 {
				t.Fatalf("duplicate removal after restart did not admit retry: %v", tracker.enqueued)
			}
			restart()
			second := first.Add(time.Hour)
			tracker.removedAt = &second
			tracker.updateErr = errors.New("tracker unavailable")
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, second.Add(time.Minute))
			restart()
			tracker.updateErr = nil
			tracker.removedHeads[issue.ID] = "merge-group-before-commit"
			got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, second.Add(time.Minute))
			if got[0].State != "Rework" || len(tracker.enqueued) != 1 {
				t.Fatalf("restart bypassed budget: state=%s enqueues=%v", got[0].State, tracker.enqueued)
			}
			restart()
			third := second.Add(time.Hour)
			tracker.removedAt = &third
			tracker.removedHeads[issue.ID] = issue.PullRequest.HeadSHA
			got = orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, third.Add(time.Minute))
			if got[0].State != "Rework" || len(tracker.enqueued) != 1 {
				t.Fatalf("budget transition reset durable count: state=%s removals=%v", got[0].State, state.nativeMergeQueueRemovals)
			}
			restart()
			issue.PullRequest.HeadSHA = "repaired-head"
			tracker.removedHeads[issue.ID] = "repaired-head"
			fourth := third.Add(time.Hour)
			tracker.removedAt = &fourth
			got = orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, fourth.Add(time.Minute))
			if got[0].State != "Merging" || len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]) != 1 {
				t.Fatalf("repaired head inherited durable failures: state=%s removals=%v", got[0].State, state.nativeMergeQueueRemovals)
			}
		})
	}
}

// Fail storage operations without changing the real timeline semantics.
type nativeQueueFailingMetrics struct {
	*autoPromoteWorkflowMetricsRecorder
	readErr, writeErr error
}

func (m *nativeQueueFailingMetrics) IssueWorkflowTimeline(ctx context.Context, identity store.IssueIdentity) (store.WorkflowTimeline, error) {
	if m.readErr != nil {
		return store.WorkflowTimeline{}, m.readErr
	}
	return m.autoPromoteWorkflowMetricsRecorder.IssueWorkflowTimeline(ctx, identity)
}

func (m *nativeQueueFailingMetrics) RecordWorkflowPhaseEvent(ctx context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.autoPromoteWorkflowMetricsRecorder.RecordWorkflowPhaseEvent(ctx, event)
}

func TestNativeMergeQueueRemovalStorageFailure(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			issue := nativeMergeQueueTestIssue(2621, "success")
			removed := time.Date(2026, 9, 14, 15, 44, 41, 0, time.UTC)
			tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: "failed_checks", removedAt: &removed, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			metrics := &nativeQueueFailingMetrics{autoPromoteWorkflowMetricsRecorder: &autoPromoteWorkflowMetricsRecorder{}}
			if operation == "read" {
				metrics.readErr = errors.New("storage unavailable")
			} else {
				metrics.writeErr = errors.New("storage unavailable")
			}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: metrics}
			state := newState(cfg)
			for range 2 {
				got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, removed.Add(time.Minute))
				if got[0].State != "Merging" || len(tracker.enqueued) != 0 || len(tracker.updates) != 0 {
					t.Fatalf("storage failure mutated queue/lane: %+v", tracker)
				}
				if _, deferred := state.nativeMergeQueueDeferred[issue.ID]; !deferred {
					t.Fatal("storage failure was not deferred")
				}
			}
			metrics.readErr, metrics.writeErr = nil, nil
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, removed.Add(2*time.Minute))
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, removed.Add(3*time.Minute))
			if len(tracker.enqueued) != 1 || len(metrics.snapshot()) != 1 {
				t.Fatalf("recovered storage: enqueues=%v events=%v", tracker.enqueued, metrics.snapshot())
			}
		})
	}
}

func TestNativeMergeQueueReviewRework(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		clean           bool
		unresolved      bool
		hydratedThreads *[]connector.PullRequestReviewThread
		hydrationErr    error
		updateErr       error
		enqueueErr      error
		wantEnqueues    int
		wantRework      bool
	}{
		{name: "unresolved thread", unresolved: true, wantRework: true},
		{name: "green clean head with unresolved thread", clean: true, unresolved: true, wantRework: true},
		{name: "resolved thread", unresolved: true, hydratedThreads: &[]connector.PullRequestReviewThread{}, wantEnqueues: 1},
		{name: "newly hydrated thread", hydratedThreads: &[]connector.PullRequestReviewThread{{Path: "merge.go", Line: 10}}, wantRework: true},
		{name: "hydration unavailable", hydrationErr: errors.New("unavailable")},
		{name: "transition unavailable", unresolved: true, updateErr: errors.New("unavailable")},
		{name: "conversation rejected", enqueueErr: errors.New("enqueue github pull request: github graphql errors: Pull request A conversation must be resolved before this pull request can be merged"), wantEnqueues: 1, wantRework: true},
		{name: "transient error", enqueueErr: errors.New("service unavailable"), wantEnqueues: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			issue := nativeMergeQueueTestIssue(2643, "success")
			issue.PullRequest.MergeableState = "blocked"
			if tt.clean {
				issue.PullRequest.MergeableState = "clean"
			}
			if tt.unresolved {
				issue.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{Path: "merge.go", Line: 10}}
			}
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{updateErr: tt.updateErr}}, enqueueErr: tt.enqueueErr, hydrationErr: tt.hydrationErr, hydratedThreads: tt.hydratedThreads}
			cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging", "Rework"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			issues := orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{issue}, now)
			issues = orch.delegateNativeMergeQueueIssues(context.Background(), &state, issues, now.Add(time.Minute))
			if len(tracker.enqueued) != tt.wantEnqueues {
				t.Fatalf("enqueues = %v, want %d", tracker.enqueued, tt.wantEnqueues)
			}
			if tt.wantRework {
				if issues[0].State != "Rework" || len(tracker.updates) != 1 {
					t.Fatalf("issues=%v updates=%v, want one Rework transition", issues, tracker.updates)
				}
				if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "unresolved_review_threads") {
					t.Fatalf("comments=%v, want review handoff", tracker.comments)
				}
			} else if tt.updateErr != nil {
				if issues[0].State != "Merging" || len(tracker.comments) != 0 {
					t.Fatalf("failed transition changed issue or published handoff: %v %v", issues, tracker.comments)
				}
			} else if len(tracker.updates) != 0 {
				t.Fatalf("unexpected updates: %v", tracker.updates)
			}
		})
	}
}

func TestNativeMergeQueueConflictRework(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, message, mergeable string
		rework                   bool
		preflight                bool
	}{
		{"recorded rejection", "enqueue github pull request: github graphql errors: Pull request has merge conflicts and Pull request not in mergeable state", "dirty", true, true},
		{"reworded rejection", "is not mergeable", "CONFLICTING", true, true},
		{"dirty without rejection", "", "dirty", true, true},
		{"not mergeable", "Pull request not in mergeable state", "unknown", false, false},
		{"stale unknown conflicts", "Pull request has merge conflicts", "unknown", true, false},
		{"clean conflicts", "Pull request has merge conflicts", "clean", false, false},
		{"clean not mergeable", "Pull request not in mergeable state", "clean", false, false},
		{"queue unavailable", "queue unavailable", "unknown", false, false},
		{"secondary rate limit", "secondary rate limit", "clean", false, false},
	} {
		for _, rebased := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rebased=%t", tt.name, rebased), func(t *testing.T) {
				issue := nativeMergeQueueTestIssue(2861, "success")
				issue.PullRequest.MergeableState = tt.mergeable
				tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}, enqueueErr: errors.New(tt.message)}
				if tt.message == "" {
					tracker.enqueueErr = nil
				}
				cfg := normalizeConfig(Config{ActiveStates: []string{"Merging", "Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}}})
				orch := &Orchestrator{cfg: cfg, connector: tracker}
				state := newState(cfg)
				now := time.Now()
				issues := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
				wantState, wantEnqueues := "Merging", 2
				if tt.rework {
					wantState, wantEnqueues = "Rework", 1
					if tt.preflight {
						wantEnqueues = 0
					}
				}
				if issues[0].State != wantState {
					t.Fatalf("first tick state = %s, want %s", issues[0].State, wantState)
				}
				issues = orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, now.Add(time.Minute))
				if len(tracker.enqueued) != wantEnqueues {
					t.Fatalf("enqueues = %d, want %d", len(tracker.enqueued), wantEnqueues)
				}
				if tt.rework {
					if len(tracker.updates) != 1 || len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "merge_conflicts") {
						t.Fatalf("updates=%v comments=%v, want merge_conflicts handoff", tracker.updates, tracker.comments)
					}
					if rebased {
						issues[0].PullRequest.HeadSHA = "rebased-head"
					}
					issues[0].PullRequest.MergeableState = "clean"
					result := orch.autoPromoteHumanReviewIssues(t.Context(), &state, issues, now.Add(2*time.Minute))
					if _, promoted := result.transitioned[issue.ID]; !promoted {
						t.Fatalf("rebased head did not promote: %v", state.AutoPromoteDecisions)
					}
					if len(tracker.updates) != 2 || tracker.updates[1].state != "Merging" || !strings.Contains(tracker.comments[len(tracker.comments)-1].body, "reason: ready") {
						t.Fatalf("updates=%v comments=%v, want ready transition to Merging", tracker.updates, tracker.comments)
					}
					issues[0].State = "Merging"
					tracker.enqueueErr = errors.New("Pull request has merge conflicts and Pull request not in mergeable state")
					before := len(tracker.enqueued)
					for tick := 3; tick <= 4; tick++ {
						issues = orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, now.Add(time.Duration(tick)*time.Minute))
					}
					if issues[0].State != "Merging" || len(tracker.updates) != 2 || len(tracker.comments) != 2 || len(tracker.enqueued) != before+2 {
						t.Fatalf("clean head bounced after promotion: state=%s updates=%v comments=%v enqueues=%d", issues[0].State, tracker.updates, tracker.comments, len(tracker.enqueued))
					}

				} else if len(tracker.updates) != 0 {
					t.Fatalf("transient failure changed lane: %v", tracker.updates)
				}
			})
		}
	}
}

func TestNativeMergeQueueHeadBudget(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		newHead bool
		want    string
	}{
		{"same head", false, "Rework"}, {"new head", true, "Merging"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2738, "success")
			first := time.Date(2026, 9, 15, 9, 3, 0, 0, time.UTC)
			tracker := &nativeMergeQueueConnector{removedHeads: map[string]string{issue.ID: issue.PullRequest.HeadSHA}, removalReason: "Required check build failed: TestCheckDoctorProjects", removedAt: &first, autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, first.Add(time.Minute))
			if tt.newHead {
				issue.PullRequest.HeadSHA = "repaired-head"
				tracker.removedHeads[issue.ID] = "repaired-head"
			}
			second := first.Add(12 * time.Minute)
			tracker.removedAt = &second
			got := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, second.Add(time.Minute))
			if got[0].State != tt.want {
				t.Fatalf("state=%s, want %s", got[0].State, tt.want)
			}
			if !tt.newHead {
				if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "TestCheckDoctorProjects") {
					t.Fatalf("comments=%v", tracker.comments)
				}
				orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, second.Add(5*time.Minute))
				if len(tracker.enqueued) != 0 {
					t.Fatalf("exhausted head enqueued: %v", tracker.enqueued)
				}
			}
		})
	}
}

func TestNativeMergeQueueCachedHeadChange(t *testing.T) {
	t.Parallel()
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(2738, "success")
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging", "Rework"}})
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			now := time.Now()
			queued := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			issue = queued[0]
			if changed {
				oldHead := issue.PullRequest.HeadSHA
				issue.PullRequest.HeadSHA = "repaired-head"
				removed := now.Add(time.Second)
				tracker.removedHeads = map[string]string{issue.ID: oldHead}
				tracker.removedAt = &removed
			}
			tracker.enqueuedAt = timePointer(now.Add(nativeMergeQueueEntryRefresh))
			for pass := 1; pass <= 2; pass++ {
				orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(pass)*nativeMergeQueueEntryRefresh))
			}
			want := 1
			if changed {
				want = 2
			}
			if len(tracker.enqueued) != want {
				t.Fatalf("enqueues=%d, want %d", len(tracker.enqueued), want)
			}
			if state.nativeMergeQueueEntries[issue.ID].HeadSHA != issue.PullRequest.HeadSHA {
				t.Fatal("cached ownership retained old head")
			}
			if len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(issue)]) != 0 {
				t.Fatal("old removal charged to current head")
			}
		})
	}
}

func TestNativeMergeQueueReviewReworkAfterEnqueue(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                                       string
		refresh, observed, resolved, fail, restart bool
		absent, merged, missingPR                  bool
		move                                       string
	}{
		{name: "cached review"},
		{name: "absent card", absent: true},
		{name: "merged PR", merged: true},
		{name: "missing PR", missingPR: true},
		{name: "refreshed review", refresh: true},
		{name: "provider observed review", refresh: true, observed: true},
		{name: "provider entry after restart", refresh: true, observed: true, restart: true},
		{name: "resolved review", resolved: true},
		{name: "dequeue failure", fail: true},
		{name: "operator rework", move: "Rework"},
		{name: "operator backlog", move: "Backlog"},
		{name: "observed operator rework", move: "Rework", observed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			issue := nativeMergeQueueTestIssue(2822, "success")
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging", "Rework"}})
			metrics := &workflowMetricsRecorderSpy{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: metrics}
			state := newState(cfg)
			issues := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			entry := *issues[0].PullRequest.MergeQueueEntry
			state.Pipeline = cloneIssues(issues)
			if tt.absent || tt.merged || tt.missingPR {
				if tt.absent {
					issues = nil
				}
				if tt.merged {
					issues[0].PullRequest.State = "MERGED"
				}
				if tt.missingPR {
					issues[0].State = "Backlog"
					issues[0].PullRequest = nil
				}
				tracker.dequeueErr = errors.New("github returned no merge queue entry")
				for pass := range 2 {
					issues = orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, now.Add(time.Duration(pass+1)*time.Minute))
					if len(tracker.dequeued) != 0 || len(state.nativeMergeQueueEntries) != 0 {
						t.Fatalf("pass %d: dequeues=%v cached=%v", pass, tracker.dequeued, state.nativeMergeQueueEntries)
					}
				}
				return
			}
			if tt.observed {
				tracker.entries = map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry}
			}
			if tt.restart {
				delete(state.nativeMergeQueueEntries, issue.ID)
				issues[0].PullRequest.MergeQueueEntry = nil
			}
			if tt.fail {
				tracker.dequeueErr = errors.New("unavailable")
			}
			want := "Rework"
			if tt.move != "" {
				want = tt.move
				if !tt.observed {
					result := orch.applyOperatorMove(t.Context(), &state, OperatorMoveRequest{IssueID: issue.ID, FromState: "Merging", ToState: tt.move, WriteTracker: true}, now)
					if result.err != nil {
						t.Fatal(result.err)
					}
					found := false
					for _, event := range metrics.events {
						if event.PhaseName == tt.move && event.Reason == "operator_move" {
							found = true
						}
					}
					if !found {
						t.Fatalf("operator transition not preserved: %+v", metrics.events)
					}
					issues = cloneIssues(state.Pipeline)
				}
				issues[0].State = tt.move
			} else {
				threads := []connector.PullRequestReviewThread{{Path: "merge.go", Line: 10}}
				if tt.resolved {
					threads = nil
					want = "Merging"
				}
				tracker.hydratedThreads = &threads
				if !tt.refresh {
					issues[0].PullRequest.UnresolvedReviewThreads = threads
				}
			}
			if tt.fail {
				want = "Merging"
			}
			next := now.Add(time.Minute)
			if tt.refresh {
				next = now.Add(nativeMergeQueueEntryRefresh)
			}
			issues = orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, next)
			if issues[0].State != want {
				t.Fatalf("lane = %s, want %s", issues[0].State, want)
			}
			wantDequeues := 1
			if tt.resolved {
				wantDequeues = 0
			}
			if len(tracker.dequeued) != wantDequeues {
				t.Fatalf("dequeues = %v, want %d", tracker.dequeued, wantDequeues)
			}
			if wantDequeues == 1 && tracker.dequeued[0].ID != entry.ID {
				t.Fatalf("dequeued wrong entry: %v", tracker.dequeued)
			}
			if tt.move != "" && len(tracker.comments) != 0 {
				t.Fatalf("operator move published review handoff: %v", tracker.comments)
			}
			if !tt.resolved && !tt.fail && nativeMergeQueueHasEntry(&state, issues[0]) {
				t.Fatal("withdrawn entry retains ownership")
			}
			if tt.fail && !nativeMergeQueueHasEntry(&state, issues[0]) {
				t.Fatal("failed dequeue discarded ownership")
			}
			if tt.fail {
				tracker.dequeueErr = nil
				issues = orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, next.Add(time.Second))
				if issues[0].State != "Rework" || nativeMergeQueueHasEntry(&state, issues[0]) {
					t.Fatal("successful retry did not withdraw and hand off")
				}
			}
		})
	}
}

func TestNativeMergeQueueWithdrawalDoesNotBlockDone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		dequeueErr error
	}{
		{name: "consumed entry"},
		{name: "provider unavailable", dequeueErr: errors.New("provider unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := nativeMergeQueueTestIssue(2826, "success")
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			var logs bytes.Buffer
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			state := newState(cfg)
			now := time.Now()
			issues := orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, now)
			issues[0].Closed, issues[0].ClosedReason = true, "completed"
			// GitHub consumed the queue entry, but the tick still has an open PR snapshot.
			tracker.dequeueErr = tt.dequeueErr
			reconciled := orch.reconcileClosedCompletedIssueStatuses(t.Context(), &state, issues, now.Add(time.Second))
			if _, ok := reconciled[issue.ID]; !ok || len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
				t.Fatalf("reconciled=%v updates=%v", reconciled, tracker.updates)
			}
			if len(tracker.dequeued) != 1 {
				t.Fatalf("dequeues=%v", tracker.dequeued)
			}
			if tt.dequeueErr == nil && strings.Contains(logs.String(), "error=") {
				t.Fatalf("unexpected error: %s", &logs)
			}
			if tt.dequeueErr != nil && !strings.Contains(logs.String(), tt.dequeueErr.Error()) {
				t.Fatalf("missing withdrawal error: %s", &logs)
			}
		})
	}
}

func TestNativeMergeQueueSkippedChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		status     string
		conclusion string
		missing    bool
		want       bool
	}{
		{name: "skipped required checks permit enqueue", status: "completed", conclusion: "skipped", want: true},
		{name: "running required check", status: "in_progress"},
		{name: "failed required check", status: "completed", conclusion: "failure"},
		{name: "cancelled required check", status: "completed", conclusion: "cancelled"},
		{name: "missing required context", status: "completed", conclusion: "skipped", missing: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(401, "pending")
			issue.PullRequest.Checks = []connector.PullRequestCheck{
				{Name: "Test", Status: tt.status, Conclusion: tt.conclusion},
				{Name: "Review", Status: "completed", Conclusion: "success"},
				{Name: "external", Status: "success", Conclusion: "success"},
			}
			name := "Test"
			if tt.missing {
				name = "Missing"
			}
			issue.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: name, Status: "pending"}}
			if got := nativeMergeQueueCandidate(issue, nativeMergeQueueTestConfig(Config{})); got != tt.want {
				t.Fatalf("candidate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAttemptTriageSkippedChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		aggregate  string
		conclusion string
		want       string
	}{
		{"unrun head with green aggregate", "pass", "skipped", "CI: `not fully verified (checks skipped; aggregate: pass)`"},
		{"unrun required head", "pending", "skipped", "CI: `not fully verified (checks skipped; aggregate: pending)`"},
		{"real successful run", "pass", "success", "CI: `pass`"},
		{"real failed run", "fail", "failure", "CI: `fail`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := nativeMergeQueueTestIssue(401, tt.aggregate)
			for _, name := range []string{"Lint", "Verify (ubuntu-latest)", "Test Coverage", "Browser Visual"} {
				issue.PullRequest.Checks = append(issue.PullRequest.Checks, connector.PullRequestCheck{Name: name, Status: "completed", Conclusion: tt.conclusion})
			}
			evidence := attemptTriageObservedEvidence(issue, time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC))
			if !strings.Contains(evidence, tt.want) {
				t.Fatalf("evidence = %s, want %s", evidence, tt.want)
			}
		})
	}
}
