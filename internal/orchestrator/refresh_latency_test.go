package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/store/storetest"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// Replay the actual authorization and blocked owners with many excluded active
// cards and genuinely unresolved dependencies, using SQLite rather than a no-op
// evidence writer. Counters separate store time from provider fanout.
func BenchmarkRefreshMaintenance(b *testing.B) {
	backend := storetest.Open(b)
	metrics := &maintenanceStore{Store: backend}
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "latency"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"owned"}}}, DependencyAutoUnblock: DependencyAutoUnblockConfig{Enabled: true}})
	var excluded, blocked []connector.Issue
	for i := range 100 {
		excluded = append(excluded, connector.Issue{ID: fmt.Sprintf("excluded-%d", i), Identifier: fmt.Sprintf("owner/repo#%d", i+1), State: "In Progress"})
		issue := connector.Issue{ID: fmt.Sprintf("blocked-%d", i), Identifier: fmt.Sprintf("owner/repo#%d", i+101), State: "Blocked", Labels: []string{"owned"}, BlockedBy: []connector.BlockedRef{{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress", Source: connector.BlockedRefSourceNative}}, DependencySource: connector.BlockedRefSourceNative}
		blocked = append(blocked, issue)
		_, err := backend.RecordWorkflowPhaseEvent(b.Context(), store.WorkflowPhaseEvent{ProjectID: "latency", IssueID: issue.ID, Identifier: issue.Identifier, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Blocked", Status: "entered", StartedAt: time.Now().Add(-time.Hour)})
		if err != nil {
			b.Fatal(err)
		}
	}
	tracker := &autoPromoteTickConnector{stateIssues: blocked, resolvedIssues: append(cloneIssues(blocked), connector.Issue{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress"})}
	orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: metrics, workflowMetrics: metrics}
	for _, phase := range []string{"authorize", "retired", "dependencies"} {
		b.Run(phase, func(b *testing.B) {
			metrics.reads, metrics.writes, metrics.readTime, metrics.writeTime = 0, 0, 0, 0
			metrics.batches = 0
			tracker.fetchComments = nil
			tracker.fetchIdentifiers = nil
			cycles := 0
			b.ReportAllocs()
			for b.Loop() {
				state := newState(cfg)
				switch phase {
				case "authorize":
					previous := tickPreviousState{pipeline: excluded}
					orch.filterAuthorizedTickIssues(b.Context(), &state, tickFetchedIssues{candidates: excluded, status: excluded}, &previous, time.Now())
				case "retired":
					orch.operatorReturnRetiredParks(b.Context(), &state, blocked, time.Now())
				case "dependencies":
					orch.operatorClearClosedDependencies(b.Context(), &state, blocked, time.Now())
				}
				cycles++
			}
			b.ReportMetric(float64(metrics.reads)/float64(cycles), "timeline_reads/op")
			b.ReportMetric(float64(metrics.writes)/float64(cycles), "decision_writes/op")
			b.ReportMetric(float64(metrics.batches)/float64(cycles), "batch_commits/op")
			b.ReportMetric(float64(metrics.readTime.Nanoseconds())/float64(cycles), "store_read_ns/op")
			b.ReportMetric(float64(metrics.writeTime.Nanoseconds())/float64(cycles), "store_write_ns/op")
			b.ReportMetric(float64(len(tracker.fetchComments))/float64(cycles), "comment_calls/op")
			b.ReportMetric(float64(len(tracker.fetchIdentifiers))/float64(cycles), "identity_calls/op")
		})
	}
}

type maintenanceStore struct {
	store.Store
	batches             int
	reads, writes       int
	readTime, writeTime time.Duration
}

func (s *maintenanceStore) IssueWorkflowTimeline(ctx context.Context, id store.IssueIdentity) (store.WorkflowTimeline, error) {
	start := time.Now()
	s.reads++
	v, e := s.Store.IssueWorkflowTimeline(ctx, id)
	s.readTime += time.Since(start)
	return v, e
}
func (s *maintenanceStore) RecordSchedulerDecision(ctx context.Context, d store.SchedulerDecision) (int64, error) {
	start := time.Now()
	s.writes++
	v, e := s.Store.RecordSchedulerDecision(ctx, d)
	s.writeTime += time.Since(start)
	return v, e
}

func (s *maintenanceStore) RecordSchedulerDecisions(ctx context.Context, d []store.SchedulerDecision) ([]int64, error) {
	start := time.Now()
	s.batches++
	s.writes += len(d)
	ids, e := s.Store.(store.SchedulerDecisionBatchStore).RecordSchedulerDecisions(ctx, d)
	s.writeTime += time.Since(start)
	return ids, e
}

// Catch per-card autocommit regression and lost refusals beyond the bounded live
// snapshot; every exclusion must still have durable per-issue evidence.
func TestAuthorizationBatchRetainsEveryRefusal(t *testing.T) {
	backend := storetest.Open(t)
	evidence := &maintenanceStore{Store: backend}
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "latency"}, ActiveStates: []string{"In Progress"}, Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"owned"}}}})
	state := newState(cfg)
	var issues []connector.Issue
	for i := range 300 {
		issues = append(issues, connector.Issue{ID: fmt.Sprintf("excluded-%d", i), State: "In Progress"})
	}
	previous := tickPreviousState{pipeline: issues}
	orch := &Orchestrator{cfg: cfg, workAttempts: evidence}
	got := orch.filterAuthorizedTickIssues(t.Context(), &state, tickFetchedIssues{candidates: issues, status: issues}, &previous, time.Now())
	if len(got.candidates) != 0 || len(got.status) != 0 || len(previous.pipeline) != 0 || evidence.batches != 1 || evidence.writes != 300 {
		t.Fatalf("authorization batch: candidates=%d status=%d retained=%d batches=%d writes=%d", len(got.candidates), len(got.status), len(previous.pipeline), evidence.batches, evidence.writes)
	}
	reader := backend.(store.IssueSchedulerDecisionStore)
	for _, issue := range issues {
		rows, err := reader.ListIssueSchedulerDecisions(t.Context(), store.IssueSchedulerDecisionQuery{Identity: store.IssueIdentity{ProjectID: "latency", IssueID: issue.ID}})
		if err != nil || len(rows) != 1 || rows[0].Reason != dispatchSkipAuthorizationSelector || rows[0].MetadataJSON == "{}" {
			t.Fatalf("lost refusal for %s: %v %v", issue.ID, rows, err)
		}
	}
}

// Hold the actual dependency provider call indefinitely. Eligible unrelated
// implementation must already have been admitted when maintenance reaches it.
func TestTickDispatchPrecedesBlockedMaintenance(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("priority_%t", early), func(t *testing.T) {
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "latency"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 2, DependencyAutoUnblock: DependencyAutoUnblockConfig{Enabled: true}})
			ready := connector.Issue{ID: "ready", AssignedToWorker: true, Title: "Approved implementation", Identifier: "owner/repo#1", State: "In Progress"}
			dependent := connector.Issue{ID: "dependent", AssignedToWorker: true, Title: "Unresolved implementation", Identifier: "owner/repo#2", State: "In Progress", BlockedBy: []connector.BlockedRef{{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress", Source: connector.BlockedRefSourceNative}}}
			issues := []connector.Issue{ready, dependent}
			for i := range 100 {
				issues = append(issues, connector.Issue{ID: fmt.Sprintf("blocked-%03d", i), Identifier: fmt.Sprintf("owner/repo#%d", i+100), State: "Blocked", BlockedBy: dependent.BlockedBy, DependencySource: connector.BlockedRefSourceNative})
			}
			base := &autoPromoteTickConnector{stateIssues: issues, resolvedIssues: append(cloneIssues(issues), connector.Issue{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress"})}
			state := newState(cfg)
			state.BoardIssues = cloneIssues(issues)
			state.dependencyUnblockEarly = early
			held := make(chan struct{})
			release := make(chan struct{})
			finished := make(chan struct{})
			tracker := &heldMaintenanceConnector{autoPromoteTickConnector: base, entered: held, release: release, observe: func() {
				if _, ok := state.Running[ready.ID]; !ok {
					t.Error("approved implementation was still waiting when full blocked maintenance began")
				}
				if _, ok := state.Running[dependent.ID]; ok {
					t.Error("unresolved dependent dispatched")
				}
			}, priority: early}
			runner := newWorkerHostRunner()
			orch := &Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion, 2)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			orch.publishState(ctx, &state)
			orch.startTick(&state, time.Now())
			go func() {
				defer close(finished)
				defer orch.finishTick(ctx, &state)
				orch.tick(ctx, &state, time.Now())
			}()
			defer func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(10 * time.Second):
					t.Error("tick did not finish after cancellation")
				}
			}()
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("maintenance did not reach held provider")
			}
			readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
			published, err := orch.State(readCtx)
			cancelRead()
			if err != nil {
				t.Fatalf("published runtime blocked behind provider: %v", err)
			}
			if _, ok := published.Running[ready.ID]; !ok {
				t.Fatal("admitted worker was not published during blocked maintenance")
			}
			if _, ok := published.Running[dependent.ID]; ok {
				t.Fatal("published an unresolved dependent as running")
			}
			request := receiveWorkerHostRunRequest(t, runner.started)
			if request.Issue.ID != ready.ID {
				t.Fatalf("dispatched %s", request.Issue.ID)
			}
			close(release)
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("tick did not finish")
			}
			cancel()
			if len(base.updates) != 0 {
				t.Fatalf("unresolved cards advanced: %v", base.updates)
			}
		})
	}
}

type heldMaintenanceConnector struct {
	*autoPromoteTickConnector
	entered  chan struct{}
	release  <-chan struct{}
	observe  func()
	priority bool
	held     bool
}

func (c *heldMaintenanceConnector) FetchIssueStatesByIdentifiers(ctx context.Context, refs []string) ([]connector.Issue, error) {
	// The one pre-fetch priority recovery may look up its own issue and dependency.
	// All other blocked-card self hydration belongs to the full maintenance pass.
	if len(refs) == 1 && refs[0] != "owner/repo#1000" && !c.held {
		if c.priority {
			c.priority = false
		} else {
			c.held = true
			c.observe()
			close(c.entered)
			select {
			case <-c.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return c.autoPromoteTickConnector.FetchIssueStatesByIdentifiers(ctx, refs)
}

// Catch admission using an active candidate when the same fresh status batch
// says Blocked, and stale project-status evidence delaying a released candidate.
func TestDispatchCandidateStatusBeforeMaintenance(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(fmt.Sprintf("blocked_%t", blocked), func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1})
			issue := dispatchTestIssue("ready", "In Progress")
			issue.AssignedToWorker = true
			status := cloneIssue(issue)
			if blocked {
				status.State = "Blocked"
			}
			state := newState(cfg)
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus, Reason: "blocked by project status"}
			runner := newWorkerHostRunner()
			orch := &Orchestrator{cfg: cfg, connector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion, 1)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			orch.dispatchTickIssues(ctx, &state, tickFetchedIssues{candidates: []connector.Issue{issue}, status: []connector.Issue{status}, statusOK: true}, tickTransitionRefresh{blockedRefreshOK: true}, tickPreviousState{}, nil, time.Now(), nil)
			_, running := state.Running[issue.ID]
			if running == blocked {
				t.Fatalf("running=%t with current blocked status=%t; decisions=%+v", running, blocked, state.SchedulerDecisions)
			}
		})
	}
}

func TestRetiredParkReferenceCohort(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	for _, mode := range []string{"id-only", "inline", "fresh", "missing", "failure", "partial failure", "independent failure", "independent forbidden", "discovery failure", "missing identity", "human", "budget", "reserve", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			paths := map[string]int{}
			graphQLRequests := 0
			var requestMu sync.Mutex
			independentFailure := mode == "independent failure" || mode == "independent forbidden"
			closed := independentFailure
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestMu.Lock()
				defer requestMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
					var request struct {
						Query string
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !strings.HasPrefix(request.Query, "query DetentGitHubCandidateHydration(") || !strings.Contains(request.Query, "blockedBy(") {
						t.Errorf("unexpected authority query %s", request.Query)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					graphQLRequests++
					fmt.Fprint(w, `{"errors":[{"message":"Field 'blockedBy' doesn't exist on type 'Issue'"}]}`)
					return
				}
				paths[r.URL.Path]++
				if mode == "reserve" {
					w.Header().Set("X-RateLimit-Limit", "5000")
					w.Header().Set("X-RateLimit-Remaining", "1")
					w.Header().Set("X-RateLimit-Resource", "core")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				}
				switch {
				case strings.Contains(r.URL.Path, "/dependencies/blocked_by"), strings.HasSuffix(r.URL.Path, "/pulls"):
					if mode == "discovery failure" && strings.HasSuffix(r.URL.Path, "/pulls") {
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprint(w, `{"message":"unavailable"}`)
						return
					}
					fmt.Fprint(w, `[]`)
				case strings.Contains(r.URL.Path, "/issues/"):
					if mode == "missing identity" && strings.HasSuffix(r.URL.Path, "/issues/2") {
						fmt.Fprint(w, `{}`)
						return
					}
					if mode == "failure" || (mode == "partial failure" || independentFailure) && strings.HasSuffix(r.URL.Path, "/issues/2") {
						status := http.StatusInternalServerError
						if mode == "independent forbidden" {
							status = http.StatusForbidden
						}
						w.WriteHeader(status)
						fmt.Fprint(w, `{"message":"unavailable"}`)
						return
					}
					if mode == "missing" {
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprint(w, `{"message":"not found"}`)
						return
					}
					number := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					state := "open"
					if closed || mode == "human" {
						state = "closed"
					}
					labels := `[{"name":"detent:in-progress"}]`
					if mode == "human" {
						labels = `[{"name":"human-owned"}]`
					}
					fmt.Fprintf(w, `{"node_id":%q,"number":%s,"state":%q,"body":"fresh body","html_url":%q,"labels":%s}`, r.URL.Path, number, state, "https://github.com/"+strings.TrimPrefix(r.URL.Path, "/repos/"), labels)
				default:
					t.Errorf("unexpected authority read %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			cap := 50
			if mode == "budget" {
				cap = 2
			}
			github, err := githubconnector.NewConnector(githubconnector.Config{Endpoint: server.URL + "/graphql", APIKey: "cohort-" + t.TempDir(), HTTPClient: server.Client(), Repository: "owner/repo", GitHubStatusSource: githubconnector.GitHubStatusSourceLabel, RESTFanoutMaxRequests: cap, RESTMinRemainingReserve: 2})
			if err != nil {
				t.Fatal(err)
			}
			tracker := &phaseReferenceConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, resolver: github}
			cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			if mode == "id-only" || mode == "inline" {
				ref := connector.BlockedRef{ID: "native-id", State: "Done", TrackerState: connector.BlockedRefTrackerStateClosed, HumanOwned: true, HumanCompletionReady: true, Source: connector.BlockedRefSourceNative}
				if mode == "inline" {
					ref.Identifier = "owner/repo#1"
					orch.connector = struct{ connector.Connector }{tracker}
				}
				issue := connector.Issue{ID: "root", State: "Blocked", BlockedBy: []connector.BlockedRef{ref}}
				blockers, err := orch.resolveDependencyBlockersWithError(t.Context(), issue)
				if err != nil || len(blockers) != 1 || blockers[0].Ref != ref || !dependencyBlockerReady(blockers[0], normalizeDependencyAutoUnblockConfig(cfg.DependencyAutoUnblock), cfg.TerminalStates) {
					t.Fatalf("inline authority changed: %+v, error %v", blockers, err)
				}
				if mode == "id-only" {
					evidence := orch.resolveBlockedRecoveryDependencies(t.Context(), nil, []connector.Issue{issue})[issue.ID]
					if evidence == nil || len(evidence.blockers) != 1 || evidence.blockers[0].Ref != ref {
						t.Fatalf("ID-only phase authority changed: %+v", evidence)
					}
				}
				if len(tracker.fetchIdentifiers) != 0 || len(paths) != 0 || graphQLRequests != 0 {
					t.Fatalf("inline authority read remote: %v %v, GraphQL requests %d", tracker.fetchIdentifiers, paths, graphQLRequests)
				}
				return
			}
			var roots []connector.Issue
			for i, refs := range [][]string{{"owner/repo#1"}, {"owner/repo#1", "owner/repo#2"}, {"owner/other#1"}} {
				issue := connector.Issue{ID: fmt.Sprint("root-", i), Identifier: fmt.Sprintf("owner/repo#%d", i+100), State: "Blocked", DependencySource: connector.BlockedRefSourceNative}
				for _, ref := range refs {
					issue.BlockedBy = append(issue.BlockedBy, connector.BlockedRef{Identifier: ref, State: "Done", TrackerState: connector.BlockedRefTrackerStateClosed, HumanOwned: true, HumanCompletionReady: true, Source: connector.BlockedRefSourceNative})
				}
				roots = append(roots, issue)
			}
			roots[0].WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{{Identifier: "owner/repo#1", Owner: workpad.BlockerOwnerOrchestrator, Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: "owner/repo#1", States: []string{"open"}}}}}
			roots[2].WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{{Identifier: "owner/repo#2", Owner: workpad.BlockerOwnerOrchestrator, Predicate: &workpad.Predicate{Type: workpad.PredicatePullRequestState, Identifier: "owner/repo#2", States: []string{"missing"}}}}}
			ctx := t.Context()
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			state := newState(cfg)
			got := orch.operatorReturnRetiredParks(ctx, &state, roots, time.Now())
			if independentFailure {
				if len(got) != 1 || len(tracker.updates) != 1 || tracker.updates[0].issueID != roots[0].ID {
					t.Fatalf("independent closed predicate did not recover: %v, updates %v", got, tracker.updates)
				}
				for _, root := range roots[1:] {
					if held, ok := state.Blocked[root.ID]; !ok || held.RecoveryAction == "transition" {
						t.Fatalf("unavailable reference released %s: %+v", root.ID, held)
					}
				}
				if len(tracker.fetchIdentifiers) < 2 || fmt.Sprint(tracker.fetchIdentifiers[1]) != "[owner/repo#1]" {
					t.Fatalf("independent authority was not freshly resolved: %v", tracker.fetchIdentifiers)
				}
				return
			}
			if len(got) != 0 || len(tracker.updates) != 0 {
				t.Fatalf("unavailable/open reference advanced roots: %v %v", got, tracker.updates)
			}
			requestMu.Lock()
			requests := maps.Clone(paths)
			graphqlCount := graphQLRequests
			requestMu.Unlock()
			want := []string{"owner/repo#1", "owner/repo#2", "owner/other#1"}
			if len(tracker.fetchIdentifiers) == 0 || fmt.Sprint(tracker.fetchIdentifiers[0]) != fmt.Sprint(want) {
				t.Fatalf("resolver calls = %v, requests = %v; want first ordered deduplicated cohort %v", tracker.fetchIdentifiers, requests, want)
			}
			failedCohort := mode == "failure" || mode == "partial failure" || mode == "discovery failure" || mode == "budget" || mode == "reserve" || mode == "cancelled"
			if !failedCohort && len(tracker.fetchIdentifiers) != 1 || failedCohort && len(tracker.fetchIdentifiers) <= 1 {
				t.Fatalf("resolver calls = %v, requests = %v; failed cohort=%t", tracker.fetchIdentifiers, requests, failedCohort)
			}
			if mode == "fresh" {
				if graphqlCount != 3 {
					t.Fatalf("GraphQL requests = %d, want 3 scheduler hydration fallbacks", graphqlCount)
				}
				if len(requests) != 8 {
					t.Fatalf("requests = %v, want 3 issues + 3 dependencies + 2 repository lists", requests)
				}
				for path, count := range requests {
					if count != 1 {
						t.Fatalf("repeated %s: %d", path, count)
					}
				}
				for _, root := range roots {
					if held, ok := state.Blocked[root.ID]; !ok || held.RecoveryAction == "transition" {
						t.Fatalf("missing per-root hold for %s: %+v", root.ID, held)
					}
				}
				requestMu.Lock()
				closed = true
				requestMu.Unlock()
				orch.operatorReturnRetiredParks(ctx, &state, roots, time.Now())
				if len(tracker.fetchIdentifiers) != 5 || len(tracker.updates) != 1 || tracker.updates[0].issueID != roots[0].ID {
					t.Fatalf("expected fresh phase plus per-root reads after first lane write: %v, updates %v", tracker.fetchIdentifiers, tracker.updates)
				}
				for _, root := range roots[1:] {
					for _, ref := range state.Blocked[root.ID].Issue.BlockedBy {
						if ref.TrackerState != connector.BlockedRefTrackerStateClosed || ref.Source != connector.BlockedRefSourceNative {
							t.Fatalf("stale mapping for %s: %+v", root.ID, ref)
						}
					}
				}
			} else if mode != "human" {
				for _, root := range roots {
					held, ok := state.Blocked[root.ID]
					if !ok {
						t.Fatalf("lost hold for %s", root.ID)
					}
					for _, ref := range held.Issue.BlockedBy {
						if ref.State == "Done" || ref.TrackerState == connector.BlockedRefTrackerStateClosed || ref.HumanCompletionReady {
							t.Fatalf("retained stale passable evidence for %s: %+v", root.ID, ref)
						}
					}
				}
				if mode == "budget" || mode == "reserve" {
					if mode == "reserve" {
						cap = 2
					}
					total := 0
					for _, count := range requests {
						total += count
					}
					if total != cap {
						t.Fatalf("request cap changed: %v", requests)
					}
					if graphqlCount != 1 {
						t.Fatalf("GraphQL requests = %d, want 1 scheduler hydration fallback", graphqlCount)
					}
				}
				if mode == "cancelled" && (len(requests) != 0 || graphqlCount != 0) {
					t.Fatalf("cancelled phase read remote: %v, GraphQL requests %d", requests, graphqlCount)
				}
			}
		})
	}
}

type phaseReferenceConnector struct {
	*autoPromoteTickConnector
	resolver connector.IssueReferenceResolver
}

func (c *phaseReferenceConnector) FetchIssueStatesByIdentifiers(ctx context.Context, refs []string) ([]connector.Issue, error) {
	c.fetchIdentifiers = append(c.fetchIdentifiers, append([]string(nil), refs...))
	return c.resolver.FetchIssueStatesByIdentifiers(ctx, refs)
}
