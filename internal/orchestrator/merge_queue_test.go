package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
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

func TestDelegateNativeMergeQueueIssuesHonorsAgedHeadReservation(t *testing.T) {
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
			name:           "retrying aged head protects same repository",
			sameRepository: true,
			setup: func(state *State) {
				state.Retry[aged.ID] = Retry{Issue: aged, DueAt: now.Add(time.Minute)}
			},
		},
		{
			name:           "CI reservation protects same repository",
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

			if len(tracker.enqueued) != tt.wantEnqueued || tracker.inspections != tt.wantEnqueued {
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
	runner := newWorkerHostRunner()
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.enqueued, []string{"issue-111", "issue-112", "issue-113"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued issue ids = %#v, want %#v", got, want)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected merge worker dispatch for %q", request.Issue.ID)
	default:
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

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(201, "success")
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
	second := orch.delegateNativeMergeQueueIssues(context.Background(), &state, first, now.Add(10*time.Second))

	if tracker.inspections != 1 || len(tracker.enqueued) != 1 {
		t.Fatalf("queue calls = %d inspections and %d enqueues, want one each", tracker.inspections, len(tracker.enqueued))
	}
	if second[0].PullRequest == nil || second[0].PullRequest.MergeQueueEntry == nil {
		t.Fatalf("cached queue entry missing from %#v", second[0].PullRequest)
	}
}

func TestDelegateNativeMergeQueueIssuesReenqueuesMissingCachedEntry(t *testing.T) {
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

	if tracker.inspections != 2 || len(tracker.enqueued) != 2 {
		t.Fatalf("queue calls = %d inspections and %d enqueues, want two each after entry disappears", tracker.inspections, len(tracker.enqueued))
	}
	if second[0].PullRequest == nil || second[0].PullRequest.MergeQueueEntry == nil || second[0].PullRequest.MergeQueueEntry.State == "MISSING" {
		t.Fatalf("re-enqueued queue entry = %#v, want active replacement", second[0].PullRequest)
	}
}

func TestDelegateNativeMergeQueueIssuesFallsBackWhenCachedQueueDisappears(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(203, "success")
	available := true
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

	first := orch.delegateNativeMergeQueueIssues(context.Background(), &state, []connector.Issue{issue}, now)
	available = false
	second := orch.delegateNativeMergeQueueIssues(context.Background(), &state, first, now.Add(nativeMergeQueueEntryRefresh))

	if len(tracker.enqueued) != 1 {
		t.Fatalf("enqueued = %#v, want no enqueue after queue disappears", tracker.enqueued)
	}
	if second[0].PullRequest == nil || second[0].PullRequest.MergeQueueEntry != nil {
		t.Fatalf("pull request = %#v, want stale queue entry cleared", second[0].PullRequest)
	}
	candidates := orch.mergeWorkerDispatchCandidates(&state, second, now.Add(nativeMergeQueueEntryRefresh))
	if len(candidates) != 1 || candidates[0].ID != issue.ID {
		t.Fatalf("merge worker candidates = %#v, want fallback issue %q", candidates, issue.ID)
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

func TestReconcileUnsafeNativeMergeQueueIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	queueEntry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-403", State: "QUEUED"}
	tests := []struct {
		name           string
		gateKind       string
		entry          *connector.PullRequestMergeQueueEntry
		inspectErr     error
		dequeueErr     error
		wantCandidates int
		wantQueued     bool
		wantDequeued   int
	}{
		{name: "dispatches when no native entry exists", wantCandidates: 1},
		{name: "dequeues existing native entry before dispatch", entry: &queueEntry, wantCandidates: 1, wantDequeued: 1},
		{name: "dequeues existing native entry for human review gate", gateKind: gate.KindHumanReview, entry: &queueEntry, wantCandidates: 1, wantDequeued: 1},
		{name: "defers when queue inspection fails", inspectErr: errors.New("inspect unavailable")},
		{name: "defers when dequeue fails", entry: &queueEntry, dequeueErr: errors.New("dequeue unavailable"), wantQueued: true, wantDequeued: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := nativeMergeQueueTestIssue(403, "success")
			gateKind := tt.gateKind
			if gateKind == "" {
				gateKind = gate.KindCommand
			}
			if gateKind == gate.KindHumanReview {
				issue.Labels = append(issue.Labels, gate.DefaultApprovalLabel)
			}
			tracker := &nativeMergeQueueConnector{
				autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
					autoPromoteTickConnector: &autoPromoteTickConnector{},
				},
				inspectErr: tt.inspectErr,
				dequeueErr: tt.dequeueErr,
			}
			if tt.entry != nil {
				tracker.entries = map[string]connector.PullRequestMergeQueueEntry{issue.ID: *tt.entry}
			}
			cfg := normalizeConfig(Config{
				MergeFastPathEnabled: false,
				MaxConcurrentAgentsByState: map[string]int{
					"Merging": 1,
				},
				AutoPromote:  AutoPromoteConfig{Gate: gate.Config{Kind: gateKind}},
				ActiveStates: []string{"Merging"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)

			queued := orch.reconcileUnsafeNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, nil, now)

			if tracker.inspections != 1 || len(tracker.enqueued) != 0 {
				t.Fatalf("native queue activity = %d inspections and %#v enqueues, want one inspection and no enqueue", tracker.inspections, tracker.enqueued)
			}
			if got := len(tracker.dequeued); got != tt.wantDequeued {
				t.Fatalf("dequeue calls = %d, want %d", got, tt.wantDequeued)
			}
			queuedEntry := queued[0].PullRequest.MergeQueueEntry
			if (queuedEntry != nil) != tt.wantQueued {
				t.Fatalf("queued entry = %#v, want present %t", queuedEntry, tt.wantQueued)
			}
			candidates := orch.mergeWorkerDispatchCandidates(&state, queued, now)
			if len(candidates) != tt.wantCandidates {
				t.Fatalf("merge worker candidates = %#v, want %d", candidates, tt.wantCandidates)
			}
			if tt.wantCandidates == 1 && candidates[0].ID != issue.ID {
				t.Fatalf("merge worker candidate = %#v, want issue %q", candidates[0], issue.ID)
			}
		})
	}
}

func TestTickReconcilesUnsafeNativeQueueBeforeStaleMerging(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		dequeueErr  error
		wantUpdates []autoPromoteTickUpdate
	}{
		{
			name:        "dequeues before approval revocation transition",
			wantUpdates: []autoPromoteTickUpdate{{issueID: "issue-404", state: "Human Review"}},
		},
		{
			name:       "defers approval revocation when dequeue fails",
			dequeueErr: errors.New("dequeue unavailable"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := nativeMergeQueueTestIssue(404, "success")
			entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-404", State: "QUEUED"}
			tracker := &nativeMergeQueueConnector{
				autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
					autoPromoteTickConnector: &autoPromoteTickConnector{
						stateIssues:        []connector.Issue{issue},
						candidateIssuesSet: true,
					},
				},
				dequeueErr: tt.dequeueErr,
				entries:    map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry},
			}
			cfg := normalizeConfig(Config{
				PollInterval:         time.Minute,
				MergeFastPathEnabled: false,
				MaxConcurrentAgents:  1,
				MaxConcurrentAgentsByState: map[string]int{
					"Merging": 1,
				},
				AutoPromote: AutoPromoteConfig{
					Enabled: true,
					Gate:    gate.Config{Kind: gate.KindHumanReview},
				},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				ObservedStates: []string{"Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)

			orch.tick(t.Context(), &state, now)

			if tracker.inspections != 1 || len(tracker.dequeued) != 1 {
				t.Fatalf("native queue activity = %d inspections and %d dequeues, want one each", tracker.inspections, len(tracker.dequeued))
			}
			if !reflect.DeepEqual(tracker.updates, tt.wantUpdates) {
				t.Fatalf("updates = %#v, want %#v", tracker.updates, tt.wantUpdates)
			}
			_, deferred := state.nativeMergeQueueDeferred[issue.ID]
			if deferred != (tt.dequeueErr != nil) {
				t.Fatalf("nativeMergeQueueDeferred[%q] present = %t, want %t", issue.ID, deferred, tt.dequeueErr != nil)
			}
		})
	}
}

func TestTickReconcilesUnsafeNativeQueueWithoutObservedStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(405, "success")
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-405", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{
				candidateIssuesSet: true,
			},
		},
		statusErr: errors.New("status unavailable"),
		entries:   map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry},
	}
	cfg := normalizeConfig(Config{
		PollInterval:         time.Minute,
		MergeFastPathEnabled: false,
		MaxConcurrentAgents:  1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate:    gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Pipeline = []connector.Issue{issue}

	orch.tick(t.Context(), &state, now)

	if tracker.inspections != 1 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want one each", tracker.inspections, len(tracker.dequeued))
	}
	if len(tracker.enqueued) != 0 {
		t.Fatalf("enqueued issues = %#v, want none", tracker.enqueued)
	}
	if len(state.Pipeline) != 1 || state.Pipeline[0].ID != issue.ID {
		t.Fatalf("pipeline = %#v, want prior issue %q retained", state.Pipeline, issue.ID)
	}
}

func TestTickReconcilesUnsafeNativeQueueAfterLeavingMerging(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 17, 15, 0, 0, time.UTC)
	previous := nativeMergeQueueTestIssue(406, "success")
	current := cloneIssue(previous)
	current.State = "Human Review"
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-406", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{
				stateIssues:        []connector.Issue{current},
				candidateIssuesSet: true,
			},
		},
		entries: map[string]connector.PullRequestMergeQueueEntry{previous.ID: entry},
	}
	cfg := normalizeConfig(Config{
		PollInterval:         time.Minute,
		MergeFastPathEnabled: false,
		MaxConcurrentAgents:  1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Gate: gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Pipeline = []connector.Issue{previous}
	cacheNativeMergeQueueEntry(&state, previous.ID, entry, now.Add(-time.Minute))

	orch.tick(t.Context(), &state, now)

	if tracker.inspections != 1 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want one each", tracker.inspections, len(tracker.dequeued))
	}
	if len(state.Pipeline) != 1 || state.Pipeline[0].State != current.State {
		t.Fatalf("pipeline = %#v, want current %q state retained", state.Pipeline, current.State)
	}
	if _, ok := state.nativeMergeQueueEntries[previous.ID]; ok {
		t.Fatalf("nativeMergeQueueEntries[%q] remains after dequeue", previous.ID)
	}
}

func TestReconcileUnsafeNativeQueueRetriesAfterLeavingMerging(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	previous := nativeMergeQueueTestIssue(407, "success")
	current := cloneIssue(previous)
	current.State = "Human Review"
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-407", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
		inspectErr: errors.New("inspect unavailable"),
		entries:    map[string]connector.PullRequestMergeQueueEntry{previous.ID: entry},
	}
	cfg := normalizeConfig(Config{
		AutoPromote:  AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}},
		ActiveStates: []string{"Merging"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	orch.reconcileUnsafeNativeMergeQueueIssues(
		t.Context(),
		&state,
		[]connector.Issue{current},
		[]connector.Issue{previous},
		now,
	)
	if _, ok := state.nativeMergeQueueDeferred[previous.ID]; !ok {
		t.Fatalf("nativeMergeQueueDeferred[%q] missing after inspection failure", previous.ID)
	}

	tracker.inspectErr = nil
	orch.reconcileUnsafeNativeMergeQueueIssues(
		t.Context(),
		&state,
		[]connector.Issue{current},
		nil,
		now.Add(time.Minute),
	)

	if tracker.inspections != 2 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want two inspections and one dequeue", tracker.inspections, len(tracker.dequeued))
	}
	if _, ok := state.nativeMergeQueueDeferred[previous.ID]; ok {
		t.Fatalf("nativeMergeQueueDeferred[%q] remains after successful retry", previous.ID)
	}
}

func TestReconcileNativeMergeQueueAfterSecurityAuditEnabled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 17, 45, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(408, "success")
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-408", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
		entries: map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry},
	}
	cfg := normalizeConfig(Config{
		MergeFastPathEnabled: true,
		AutoPromote: AutoPromoteConfig{Gate: gate.Config{
			Kind:          gate.KindArtifact,
			SecurityAudit: gate.SecurityAuditConfig{Enabled: true},
		}},
		ActiveStates: []string{"Merging"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	orch.reconcileUnsafeNativeMergeQueueIssues(
		t.Context(),
		&state,
		[]connector.Issue{issue},
		nil,
		now,
	)

	if tracker.inspections != 1 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want one each", tracker.inspections, len(tracker.dequeued))
	}
}

func TestReconcileNativeMergeQueueRecoversDepartedIssueAfterRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(409, "success")
	issue.State = "Rework"
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-409", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
		entries: map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry},
	}
	cfg := normalizeConfig(Config{
		MergeFastPathEnabled: true,
		AutoPromote:          AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}},
		ActiveStates:         []string{"Rework", "Merging"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	orch.reconcileUnsafeNativeMergeQueueIssues(
		t.Context(),
		&state,
		[]connector.Issue{issue},
		nil,
		now,
	)

	if tracker.inspections != 1 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want one each", tracker.inspections, len(tracker.dequeued))
	}
}

func TestReconcileNativeMergeQueueRechecksEmptyRecoveryInspection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 15, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(410, "success")
	issue.State = "Human Review"
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
	}
	cfg := normalizeConfig(Config{
		MergeFastPathEnabled: true,
		AutoPromote:          AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindHumanReview}},
		ActiveStates:         []string{"Merging"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	for _, checkedAt := range []time.Time{now, now.Add(time.Minute), now.Add(nativeMergeQueueEntryRefresh)} {
		orch.reconcileUnsafeNativeMergeQueueIssues(
			t.Context(),
			&state,
			[]connector.Issue{issue},
			nil,
			checkedAt,
		)
	}

	if tracker.inspections != 3 {
		t.Fatalf("native queue inspections = %d, want 3", tracker.inspections)
	}
	if len(tracker.dequeued) != 0 {
		t.Fatalf("dequeued entries = %#v, want none", tracker.dequeued)
	}
}

func TestTickReconcilesUnsafeNativeQueueTerminalIssueAfterRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 30, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(411, "success")
	issue.State = "Cancelled"
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-411", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{
				stateIssues:        []connector.Issue{issue},
				candidateIssuesSet: true,
			},
		},
		entries: map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry},
	}
	cfg := normalizeConfig(Config{
		PollInterval:         time.Minute,
		MergeFastPathEnabled: true,
		MaxConcurrentAgents:  1,
		AutoPromote:          AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}},
		ActiveStates:         []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:       []string{"Merging"},
		TerminalStates:       []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)

	orch.tick(t.Context(), &state, now)

	if tracker.inspections != 1 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want one each", tracker.inspections, len(tracker.dequeued))
	}
	if state.nativeQueueSweepAt != now {
		t.Fatalf("nativeQueueSweepAt = %s, want %s", state.nativeQueueSweepAt, now)
	}
}

func TestTickPeriodicallyReconcilesUnsafeNativeQueueTerminalIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 40, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(413, "success")
	issue.State = "Done"
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{
				stateIssues:        []connector.Issue{issue},
				candidateIssuesSet: true,
			},
		},
	}
	cfg := normalizeConfig(Config{
		PollInterval:         time.Minute,
		MergeFastPathEnabled: true,
		MaxConcurrentAgents:  1,
		AutoPromote:          AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}},
		ActiveStates:         []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:       []string{"Merging"},
		TerminalStates:       []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)

	orch.tick(t.Context(), &state, now)
	tracker.entries = map[string]connector.PullRequestMergeQueueEntry{
		issue.ID: {ID: "MQE_issue-413", State: "QUEUED"},
	}
	orch.tick(t.Context(), &state, now.Add(time.Minute))
	orch.tick(t.Context(), &state, now.Add(nativeMergeQueueTerminalSweep))

	if tracker.inspections != 2 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want two inspections and one dequeue", tracker.inspections, len(tracker.dequeued))
	}
	if state.nativeQueueSweepAt != now.Add(nativeMergeQueueTerminalSweep) {
		t.Fatalf("nativeQueueSweepAt = %s, want %s", state.nativeQueueSweepAt, now.Add(nativeMergeQueueTerminalSweep))
	}
}

func TestReconcileUnsafeNativeQueueRetriesTerminalIssueWithoutRefetch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 45, 0, 0, time.UTC)
	issue := nativeMergeQueueTestIssue(412, "success")
	issue.State = "Cancelled"
	entry := connector.PullRequestMergeQueueEntry{ID: "MQE_issue-412", State: "QUEUED"}
	tracker := &nativeMergeQueueConnector{
		autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{},
		},
		inspectErr: errors.New("inspect unavailable"),
		entries:    map[string]connector.PullRequestMergeQueueEntry{issue.ID: entry},
	}
	cfg := normalizeConfig(Config{
		MergeFastPathEnabled: true,
		AutoPromote:          AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}},
		TerminalStates:       []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	orch.reconcileUnsafeNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, nil, now)
	if _, ok := state.nativeQueueRetries[issue.ID]; !ok {
		t.Fatalf("nativeQueueRetries[%q] missing after inspection failure", issue.ID)
	}

	tracker.inspectErr = nil
	orch.reconcileUnsafeNativeMergeQueueIssues(t.Context(), &state, nil, nil, now.Add(time.Minute))

	if tracker.inspections != 2 || len(tracker.dequeued) != 1 {
		t.Fatalf("native queue activity = %d inspections and %d dequeues, want two inspections and one dequeue", tracker.inspections, len(tracker.dequeued))
	}
	if _, ok := state.nativeQueueRetries[issue.ID]; ok {
		t.Fatalf("nativeQueueRetries[%q] remains after successful retry", issue.ID)
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
	available      *bool
	inspectErr     error
	inspectedHead  string
	admissionLimit *int
	removedHeads   map[string]string
	removalReason  string
	dequeueErr     error
	statusErr      error
	inspections    int
	entries        map[string]connector.PullRequestMergeQueueEntry
	enqueued       []string
	dequeued       []connector.PullRequestMergeQueueEntry
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

func (c *nativeMergeQueueConnector) EnqueuePullRequest(_ context.Context, issue connector.Issue) (connector.PullRequestMergeQueueEntry, error) {
	c.enqueued = append(c.enqueued, issue.ID)
	return connector.PullRequestMergeQueueEntry{
		ID:                          "MQE_" + issue.ID,
		State:                       "QUEUED",
		Position:                    len(c.enqueued),
		Depth:                       len(c.enqueued),
		EstimatedTimeToMergeSeconds: int64(len(c.enqueued)) * 60,
		EnqueuedAt:                  timePointer(time.Now()),
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
			wantInspections := 2
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
				if !tt.manualReenqueue && !tt.wantReenqueue && queued[1].State != autoPromoteReworkState {
					t.Fatalf("removed queue issue state = %q, want Rework", queued[1].State)
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
			if got[0].State != "Rework" || len(tracker.enqueued) != 0 {
				t.Fatalf("issues=%+v enqueue=%v", got, tracker.enqueued)
			}
			if len(recorder.events) != 2 {
				t.Fatalf("events=%+v", recorder.events)
			}
			event := recorder.events[1]
			if reason != "" && event.Reason != reason {
				t.Fatalf("reason=%q", event.Reason)
			}
			if event.Reason == "" {
				t.Fatal("missing rejection reason")
			}
			metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
			if !ok || metadata.PullRequest == nil || metadata.PullRequest.CIDurationSeconds != 1320 || metadata.PullRequest.BaseRef != "main" {
				t.Fatalf("metadata=%s", event.MetadataJSON)
			}
			if len(orch.mergeWorkerDispatchCandidates(&state, got, now)) != 0 {
				t.Fatal("rejection entered merge worker CI loop")
			}
		})
	}
}
