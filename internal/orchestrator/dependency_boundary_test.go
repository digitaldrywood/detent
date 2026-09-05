package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestDependencySectionAndReferenceBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		line, title string
		valid       bool
	}{
		{line: "## Dependencies", title: "Dependencies", valid: true},
		{line: "###### Dependencies ###", title: "Dependencies", valid: true},
		{line: "####### Dependencies"}, {line: "###"}, {line: "#123"}, {line: "prose"},
	} {
		t.Run(tt.line, func(t *testing.T) {
			title, ok := dependencyMarkdownHeadingTitle(tt.line)
			if title != tt.title || ok != tt.valid {
				t.Fatalf("heading = %q %v", title, ok)
			}
		})
	}
	if got := dependencyMarkdownSectionText("# Task\nText\n## Dependencies\nDepends on: #1\n## Criteria\nIndependent work", "Dependencies"); got != "Depends on: #1" {
		t.Fatalf("section = %q", got)
	}
	if got := dependencyIssueIdentifiersInText("https://github.com/owner/repo/issues/1 owner/repo#1 #2 #2", "owner/repo"); !reflect.DeepEqual(got, []string{"owner/repo#1", "owner/repo#2"}) {
		t.Fatalf("refs = %v", got)
	}
	if dependencyBlockerIdentifier("", "", "") != "" || dependencyBlockerIdentifier("", "1", "") != "#1" {
		t.Fatal("invalid local reference normalization")
	}
	if !sameIssueIdentity(connector.Issue{Identifier: "OWNER/REPO#1"}, connector.Issue{Identifier: "owner/repo#1"}) {
		t.Fatal("canonical identity mismatch")
	}
}

func TestDependencyAuditEvidenceBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		blocker dependencyBlocker
		label   string
		ready   bool
	}{
		{name: "missing", label: "unknown dependency"},
		{name: "local id", blocker: dependencyBlocker{Ref: connector.BlockedRef{ID: "local"}}, label: "local"},
		{name: "resolved id", blocker: dependencyBlocker{Resolved: true, Issue: connector.Issue{ID: "remote"}}, label: "remote"},
		{name: "terminal", blocker: dependencyBlocker{Ref: connector.BlockedRef{Identifier: "#1", State: "Done"}}, label: "#1", ready: true},
		{name: "open", blocker: dependencyBlocker{Resolved: true, Issue: connector.Issue{Identifier: "#1", State: "Backlog"}}, label: "#1"},
		{name: "merged", blocker: dependencyBlocker{Resolved: true, Issue: connector.Issue{Identifier: "#1", PullRequest: &connector.PullRequest{Number: 40, State: "merged"}}}, label: "#1", ready: true},
		{name: "human closed without evidence", blocker: dependencyBlocker{Ref: connector.BlockedRef{Identifier: "#1", State: "Done", HumanOwned: true}}, label: "#1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := dependencyBlockerLabel(tt.blocker); got != tt.label {
				t.Fatalf("label = %q", got)
			}
			if got := dependencyBlockerReady(tt.blocker, DependencyAutoUnblockConfig{Readiness: DependencyReadinessTerminalOrMerged}, []string{"done"}); got != tt.ready {
				t.Fatalf("ready = %v", got)
			}
			comment := dependencyAutoUnblockComment("Blocked", "Todo", []dependencyBlocker{tt.blocker})
			if !strings.Contains(comment, tt.label) {
				t.Fatalf("missing evidence: %s", comment)
			}
		})
	}
	if dependencyBlockersReady(nil, DependencyAutoUnblockConfig{}, nil) || dependencyBlockersTerminal(nil, nil) {
		t.Fatal("empty dependencies ready")
	}
	for _, blocker := range []dependencyBlocker{{Ref: connector.BlockedRef{State: "Todo"}}, {Resolved: true, Issue: connector.Issue{State: "Todo"}}} {
		if dependencyBlockersTerminal([]dependencyBlocker{blocker}, []string{"done"}) {
			t.Fatal("nonterminal blocker accepted")
		}
	}
	if dependencyBlockedRefSource(connector.BlockedRef{Source: "unsupported"}) != "unknown" {
		t.Fatal("unrecognized evidence source accepted")
	}
	comment := blockerAutoPromoteComment(connector.Issue{Identifier: "#1", URL: "https://github.com/owner/repo/issues/1"}, connector.Issue{State: "Backlog"}, "Todo")
	if !strings.Contains(comment, "https://github.com/owner/repo/issues/1") {
		t.Fatal("audit omitted dependent link")
	}
}

type dependencyFailureTracker struct {
	connector.Connector
	resolveErr error
}

func (tracker dependencyFailureTracker) FetchIssueStatesByIdentifiers(context.Context, []string) ([]connector.Issue, error) {
	return nil, tracker.resolveErr
}

func TestDependencyDeferralRefreshFailuresStayWaiting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		tracker connector.Connector
	}{
		{name: "unsupported", tracker: struct{ connector.Connector }{memory.New(memory.Config{})}},
		{name: "unavailable", tracker: dependencyFailureTracker{Connector: memory.New(memory.Config{}), resolveErr: errors.New("tracker unavailable")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := implementProgressIssueWithoutPR()
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{{Identifier: "owner/repo#10"}}}
			o := &Orchestrator{cfg: normalizeConfig(Config{}), connector: tt.tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workAttempts: &recordingWorkAttemptStore{history: []store.WorkAttempt{implementProgressDependencyDeferralHistoryAttempt(1, "owner/repo#10", "Backlog")}}}
			_, rejected, deferred := o.evaluateImplementDependencyDeferral(t.Context(), issue)
			if deferred || len(rejected) != 1 {
				t.Fatalf("unverifiable deferral accepted: %v %v", rejected, deferred)
			}
			if got := o.filterImplementDependencyDeferrals(t.Context(), []connector.Issue{issue}); len(got) != 0 {
				t.Fatal("restart released unverified dependency")
			}
		})
	}
	refs := implementDependencyIdentifiers([]workpad.Blocker{{}, {Ref: "owner/repo#1"}, {Identifier: "OWNER/REPO#1"}})
	if !reflect.DeepEqual(refs, []string{"owner/repo#1"}) {
		t.Fatalf("identifiers = %v", refs)
	}
	if implementDependencyBlockerLabels([]implementDependencyBlocker{{}, {Identifier: "owner/repo#1"}}) != "owner/repo#1" {
		t.Fatal("invalid blocker labels")
	}
}

func TestDependencyRetryPreservesParks(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{FailureRetryBaseDelay: time.Second, MaxRetryBackoff: time.Minute})
	p := newDispatchPlanner(cfg)
	state := newState(cfg)
	issue := dispatchTestIssue("child", "Todo")
	now := time.Now()
	for _, attempt := range []int{-1, 0, 100} {
		p.scheduleRetry(&state, issue, attempt, now, "dependency waiting", false, "")
		if retry := state.Retry[issue.ID]; retry.Attempt < 1 || retry.DueAt.After(now.Add(time.Minute)) {
			t.Fatalf("invalid retry: %+v", retry)
		}
	}
	p.rescheduleRetry(&state, Retry{Issue: issue}, now, "dependency waiting", false)
	if state.Retry[issue.ID].Attempt != 1 {
		t.Fatal("retry attempt not normalized")
	}
	p.scheduleRetryAfter(&state, issue, -1, now, -time.Second, "waiting", "")
	if !state.Retry[issue.ID].DueAt.Equal(now) {
		t.Fatal("negative delay not bounded")
	}
	state.Blocked[issue.ID] = Blocked{Issue: issue, Reason: repeatedFailureCircuitBreakerCause}
	p.parkBudgetHardHold(&state, issue.ID)
	if _, ok := state.Blocked[issue.ID]; !ok {
		t.Fatal("budget hold discarded independent park")
	}
	if p.hardAvailableSlots(nil) != 0 || p.providerModelPermitSlots(nil) != 0 || p.rateWindowBackpressureActive(nil) || modelPermitsUsed(nil) != 0 {
		t.Fatal("missing capacity admitted a worker")
	}
	if got := p.retryDelay(100, false); got != time.Minute {
		t.Fatalf("unbounded retry = %s", got)
	}
	if got := availableSlots(&State{MaxConcurrentAgents: 0, Running: map[string]Running{"busy": {}}}); got != 0 {
		t.Fatal("negative capacity")
	}
	if got := leastLoadedWorkerHost(&State{Running: map[string]Running{"busy": {WorkerHost: "one"}}}, []string{"one", "two"}); got != "two" {
		t.Fatal("selected occupied host")
	}
	if got := normalizeWorkerHosts([]string{"one", "", " one ", "two"}); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("hosts = %v", got)
	}
}

func TestDispatchActivityEvidenceTitles(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind runner.AgentUpdateType
		want string
	}{
		{runner.AgentUpdateMessageDelta, "Agent"}, {runner.AgentUpdateToolStarted, "Tool started · test"}, {runner.AgentUpdateToolCompleted, "Tool finished · test"}, {runner.AgentUpdateMCPElicitation, "MCP elicitation · test"}, {runner.AgentUpdateTokenUsage, "Usage"}, {runner.AgentUpdateTurnStarted, "Turn started"}, {runner.AgentUpdateTurnCompleted, "Turn finished"}, {runner.AgentUpdateProcessStarted, "Worker started"}, {runner.AgentUpdateModelUpdated, "Model updated"}, {runner.AgentUpdateType("custom"), "custom"},
	} {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := activityUpdateTitle(runner.AgentActivityUpdate{Type: tt.kind, Tool: "test"}); got != tt.want {
				t.Fatalf("title = %q", got)
			}
		})
	}
	if got := activityUpdateTitle(runner.AgentActivityUpdate{Type: runner.AgentUpdateMCPElicitation}); got != "MCP elicitation" {
		t.Fatalf("title = %q", got)
	}
}

func TestDependencyResumeCIPreview(t *testing.T) {
	t.Parallel()
	for _, waiting := range []bool{false, true} {
		cfg := normalizeConfig(Config{})
		state := newState(cfg)
		issue := dispatchTestIssue("resuming", "Merging")
		issue.PullRequest = &connector.PullRequest{Number: 42, State: "open", HeadSHA: "head", MergeableState: "clean", CIStatus: "success"}
		if waiting {
			issue.PullRequest.CIStatus = "pending"
		}
		p := newDispatchPlanner(cfg)
		retry, handled := p.previewCurrentHeadCIWait(&state, issue, Retry{Issue: issue, Attempt: 1}, time.Now())
		if handled != waiting || (waiting && retry.Wait.PollCount != 1) || (!waiting && retry.Attempt != 2) {
			t.Fatalf("preview = %+v, %v", retry, handled)
		}
	}
}

func TestDependencyWorkpadEvidenceTimes(t *testing.T) {
	t.Parallel()
	at := time.Now()
	for _, tt := range []struct {
		name     string
		signal   *workpad.Signal
		comments []connector.IssueComment
		known    bool
	}{
		{name: "no signal"},
		{name: "recorded", signal: &workpad.Signal{RecordedAt: &at}, known: true},
		{name: "missing comment", signal: &workpad.Signal{CommentURL: "1"}},
		{name: "untimed comment", signal: &workpad.Signal{CommentURL: "1"}, comments: []connector.IssueComment{{URL: "1"}}},
		{name: "created comment", signal: &workpad.Signal{CommentURL: "1"}, comments: []connector.IssueComment{{URL: "other"}, {URL: "1", CreatedAt: &at}}, known: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, known := workpadSignalCommentTime(connector.Issue{Comments: tt.comments}, tt.signal)
			if known != tt.known {
				t.Fatalf("known=%v", known)
			}
		})
	}
}

func TestHumanTaskDispatchRecheckedAfterSelection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		labels  []string
		blocker bool
		want    string
	}{
		{name: "human", labels: []string{"human-owned"}, want: dispatchSkipInactiveState},
		{name: "epic", labels: []string{"epic"}, want: dispatchSkipInactiveState},
		{name: "waiting", blocker: true, want: dispatchSkipBlockedByDependency},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{cfg: normalizeConfig(Config{})}
			state := newState(o.cfg)
			issue := dispatchTestIssue("selected", "Todo")
			issue.Labels = tt.labels
			if tt.blocker {
				issue.BlockedBy = []connector.BlockedRef{{Identifier: "owner/repo#10", HumanOwned: true}}
			}
			got := o.dispatchIssueWithOutcome(t.Context(), &state, issue, 0, time.Now(), "")
			if got.dispatched || got.reason != tt.want || got.waitReason == "" {
				t.Fatalf("dispatch=%+v", got)
			}
		})
	}
}

func TestDependencyCompletionDoesNotBypassDispatchRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, lane, want string
		prepare          func(*Orchestrator, *State)
	}{
		{name: "REST capacity", lane: "Todo", want: dispatchIssueFailureGitHubRESTPaused, prepare: func(_ *Orchestrator, s *State) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: time.Now().Add(time.Hour)}
		}},
		{name: "recovery ramp", lane: "Todo", want: "test_recovery", prepare: func(_ *Orchestrator, s *State) {
			s.DispatchRecoveries["test"] = DispatchRecovery{Kind: "test", Status: dispatchRecoveryStatusRamping, ResumeAt: time.Now().Add(time.Hour)}
		}},
		{name: "draining", lane: "Todo", want: dispatchIssueFailureDraining, prepare: func(_ *Orchestrator, s *State) { s.Draining = true }},
		{name: "tracker unavailable", lane: "Todo", want: dispatchIssueFailureTrackerUnavailable, prepare: func(_ *Orchestrator, s *State) { s.TrackerUnavailable = &TrackerCondition{} }},
		{name: "CI unavailable", lane: "Merging", want: dispatchIssueFailureCIUnavailable, prepare: func(_ *Orchestrator, s *State) { s.CIUnavailable = &CICondition{} }},
		{name: "breaker", lane: "Todo", want: projectFailureBreakerDispatchPaused, prepare: func(_ *Orchestrator, s *State) {
			s.FailureBreaker.Class = "test"
			s.FailureBreaker.ResumeAt = time.Now().Add(time.Hour)
		}},
		{name: "host full", lane: "In Progress", want: dispatchIssueFailureWorkerHostUnavailable, prepare: func(o *Orchestrator, s *State) {
			o.cfg.WorkerHosts = []string{"one"}
			o.cfg.MaxConcurrentAgentsPerHost = 1
			s.Running["busy"] = Running{WorkerHost: "one"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{cfg: normalizeConfig(Config{MaxConcurrentAgents: 2, ActiveStates: []string{"Todo", "In Progress", "Merging"}}), connector: memory.New(memory.Config{})}
			state := newState(o.cfg)
			tt.prepare(o, &state)
			issue := dispatchTestIssue("released", tt.lane)
			issue.BlockedBy = []connector.BlockedRef{{Identifier: "owner/repo#10", HumanOwned: true, HumanCompletionReady: true}}
			got := o.dispatchIssueWithOutcome(t.Context(), &state, issue, 0, time.Now(), "")
			if got.dispatched || got.reason != tt.want {
				t.Fatalf("dispatch=%+v want=%s", got, tt.want)
			}
		})
	}
}

func TestDeferredWorkerCancellationAndSelection(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{cfg: normalizeConfig(Config{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.usageUpdateHandler(ctx, "child", nil)(runner.UsageUpdate{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("usage cancellation=%v", err)
	}
	if err := o.activityUpdateHandler(ctx, connector.Issue{})(runner.AgentActivityUpdate{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("activity cancellation=%v", err)
	}
	if waitForDispatchBackoff(ctx, time.Hour) {
		t.Fatal("canceled backoff succeeded")
	}
	state := newState(o.cfg)
	state.Draining = true
	o.dispatchCandidates(t.Context(), &state, []connector.Issue{dispatchTestIssue("child", "Todo")}, time.Now())
	state.Draining = false
	state.MaxConcurrentAgents = 0
	o.dispatchCandidates(t.Context(), &state, []connector.Issue{dispatchTestIssue("child", "Todo")}, time.Now())
	if len(state.Running) != 0 {
		t.Fatal("dispatch ignored missing capacity")
	}
}

type cancelUsageTimer struct{ cancel context.CancelFunc }

func (timer cancelUsageTimer) Stop() bool { timer.cancel(); return true }

func TestWorkerUsageCancellationDuringDependencyHandoff(t *testing.T) {
	t.Parallel()
	for _, update := range []runner.UsageUpdate{{DispatchLoopStart: &runner.DispatchLoopStartSnapshot{}}, {WorkerGitHubActor: connector.IssueActor{Login: "worker"}}} {
		ctx, cancel := context.WithCancel(t.Context())
		o := &Orchestrator{}
		if err := o.usageUpdateHandler(ctx, "child", cancelUsageTimer{cancel: cancel})(update); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation=%v", err)
		}
		cancel()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o := &Orchestrator{runUpdates: make(chan runUpdate)}
	done := make(chan error, 1)
	go func() {
		done <- o.usageUpdateHandler(ctx, "child", nil)(runner.UsageUpdate{DispatchLoopStart: &runner.DispatchLoopStartSnapshot{}})
	}()
	<-o.runUpdates
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handoff cancellation=%v", err)
	}
	o.dispatchClosed.Store(true)
	state := newState(normalizeConfig(Config{}))
	if got := o.dispatchIssueWithOutcome(t.Context(), &state, dispatchTestIssue("child", "Todo"), 0, time.Now(), ""); got.reason != dispatchIssueFailureDraining {
		t.Fatalf("quiesced dispatch=%+v", got)
	}
}

type dependencyResumeStartFailure struct{ recordingWorkAttemptStore }

func (*dependencyResumeStartFailure) StartWorkAttempt(context.Context, store.WorkAttemptStart) (int64, error) {
	return 0, errors.New("attempt store unavailable")
}

type dependencyResumeStateFailure struct{ *memory.Connector }

func (*dependencyResumeStateFailure) UpdateIssueState(context.Context, string, string) error {
	return errors.New("tracker state update unavailable")
}

func TestDependencyResumeFailureReleasesReservations(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, want string }{
		{name: "attempt write fails", want: dispatchIssueFailureWorkAttemptStart},
		{name: "lane write fails", want: dispatchIssueFailureStartStateTransition},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := dispatchTestIssue("released-child", "Todo")
			issue.BlockedBy = []connector.BlockedRef{{Identifier: "owner/repo#10", HumanOwned: true, HumanCompletionReady: true}}
			tracker := memory.New(memory.Config{Stateful: true, Issues: []connector.Issue{issue}})
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "test", Weight: 1}, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}})
			o := &Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: gate, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			attempts := &recordingWorkAttemptStore{}
			if tt.want == dispatchIssueFailureWorkAttemptStart {
				o.workAttempts = &dependencyResumeStartFailure{}
			} else {
				o.workAttempts = attempts
				o.connector = &dependencyResumeStateFailure{Connector: tracker}
			}
			state := newState(cfg)
			state.FailureBreaker.Class = "test"
			state.DispatchRecoveries["test"] = DispatchRecovery{Kind: "test", Status: dispatchRecoveryStatusRamping, Limit: 1}
			got := o.dispatchIssueWithOutcome(t.Context(), &state, issue, 0, time.Now(), "")
			if got.dispatched || got.reason != tt.want {
				t.Fatalf("dispatch = %+v, want %s", got, tt.want)
			}
			if len(state.Running) != 0 || state.FailureBreaker.CanaryIssueID != "" || len(state.DispatchRecoveries["test"].Admissions) != 0 {
				t.Fatal("failed resume leaked a worker or recovery reservation")
			}
			if snapshot := gate.PoolSnapshot(); snapshot.Available != 1 {
				t.Fatalf("failed resume leaked global slot: %+v", snapshot)
			}
			if tt.want == dispatchIssueFailureStartStateTransition && len(attempts.completions) != 1 {
				t.Fatal("failed lane write left durable attempt active")
			}
		})
	}
}
