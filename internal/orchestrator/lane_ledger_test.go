package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestLaneWritePersistsBeforeTrackerAndRetainsUncertainIdentity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		mutationErr error
		result      string
		origin      provenance.Origin
	}{
		{name: "applied", result: "applied", origin: provenance.OriginDetent},
		{name: "uncertain", mutationErr: errors.New("response lost"), result: "uncertain", origin: provenance.OriginDetent},
		{name: "blocked", mutationErr: connector.ErrStateUpdateBlocked, result: "blocked", origin: provenance.OriginHuman},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("intent", "example/repo#2", "Todo")
			cfg := laneMutationTestConfig()
			backend, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, at)
			tracker := ledgerCheckingConnector{update: func(ctx context.Context, id, target string) error {
				write, result, err := backend.LatestLaneWrite(ctx, store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: id})
				if err != nil || result != "prepared" || write.To != target || write.From != issue.State || write.FenceToken == 0 {
					t.Fatalf("tracker called before durable intent: %#v, %s, %v", write, result, err)
				}
				return tt.mutationErr
			}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, at)
			state := newState(cfg)
			err := orch.updateIssueStateByIDStrictWithMetadata(t.Context(), &state, issue.ID, issue, "Rework", at, "test_transition", workflowLaneMetadata{})
			if !errors.Is(err, tt.mutationErr) {
				t.Fatalf("write error=%v, want %v", err, tt.mutationErr)
			}
			_, result, err := backend.LatestLaneWrite(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
			if err != nil || result != tt.result {
				t.Fatalf("write result=%s, %v; want %s", result, err, tt.result)
			}
			issue.State = "Rework"
			_, origin, err := orch.observeLane(t.Context(), &state, issue, at.Add(time.Second))
			if err != nil || origin.Origin != tt.origin {
				t.Fatalf("acknowledgement origin=%s, %v; want %s", origin.Origin, err, tt.origin)
			}
		})
	}
}

type ledgerCheckingConnector struct {
	connector.Connector
	update func(context.Context, string, string) error
}

func (c ledgerCheckingConnector) UpdateIssueState(ctx context.Context, id, target string) error {
	return c.update(ctx, id, target)
}

func TestLaneLedgerClassifiesTransitionsWithoutStoppingWorkers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		local           bool
		peer            bool
		peerUnavailable bool
		restart         bool
		wantOrigin      provenance.Origin
		wantEvents      int
	}{
		{name: "local", local: true, wantOrigin: provenance.OriginDetent, wantEvents: 2},
		{name: "local restart", local: true, restart: true, wantOrigin: provenance.OriginDetent, wantEvents: 2},
		{name: "human", wantOrigin: provenance.OriginHuman, wantEvents: 2},
		{name: "human restart", restart: true, wantOrigin: provenance.OriginHuman, wantEvents: 2},
		{name: "peer", peer: true, wantOrigin: provenance.OriginDetent},
		{name: "peer restart", peer: true, restart: true, wantOrigin: provenance.OriginDetent},
		{name: "peer lookup unavailable", peerUnavailable: true, wantOrigin: provenance.OriginHuman, wantEvents: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("ledger", "example/repo#1", "In Progress")
			cfg := laneMutationTestConfig()
			backend, attempt := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, at)
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, backend, &recordingWorkAttemptStore{}, at)
			state := newState(cfg)
			runCtx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: attempt, Generation: 1, stop: cancel}
			if _, _, err := orch.observeLane(t.Context(), &state, issue, at); err != nil {
				t.Fatal(err)
			}
			if tt.local {
				if err := orch.updateIssueState(t.Context(), &state, issue, "Todo", at.Add(time.Second), "test_transition"); err != nil {
					t.Fatal(err)
				}
			}
			if tt.restart {
				orch = newLaneMutationTestOrchestrator(cfg, tracker, backend, &recordingWorkAttemptStore{}, at)
			}
			if tt.peer {
				write := coordination.LaneWrite{InstanceIdentity: "peer", Issue: issue.ID, From: issue.State, To: "Todo", Reason: "test_transition", FenceToken: 1, WrittenAt: at.Add(time.Second)}
				value, err := json.Marshal(map[string]map[string]coordination.LaneWrite{"writers": {"peer": write}})
				if err != nil {
					t.Fatal(err)
				}
				orch.laneCoordination = lanePeerReader{value: value}
			}
			if tt.peerUnavailable {
				orch.laneCoordination = lanePeerReader{err: errors.New("backend unavailable")}
			}
			issue.State = "Todo"
			for range 3 {
				_, origin, err := orch.observeLane(t.Context(), &state, issue, at.Add(2*time.Second))
				if err != nil || origin.Origin != tt.wantOrigin {
					t.Fatalf("observeLane() origin=%s error=%v, want %s", origin.Origin, err, tt.wantOrigin)
				}
			}
			tracker.stateIssues = []connector.Issue{issue}
			orch.reconcileRunningIssues(t.Context(), &state, at.Add(3*time.Second))
			if context.Cause(runCtx) != nil || state.Running[issue.ID].Issue.State != "Todo" {
				t.Fatalf("worker stopped or lane not routed: cause=%v running=%#v", context.Cause(runCtx), state.Running[issue.ID])
			}
			timeline, err := backend.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
			if err != nil || len(timeline.Events) != tt.wantEvents {
				t.Fatalf("lane events=%d error=%v, want %d", len(timeline.Events), err, tt.wantEvents)
			}
		})
	}
}

type lanePeerReader struct {
	err   error
	value []byte
}

func (p lanePeerReader) Get(context.Context, string) (coordination.Record, bool, error) {
	return coordination.Record{Value: p.value}, p.err == nil, p.err
}

func (p lanePeerReader) CompareAndSwap(context.Context, string, string, []byte) (coordination.Record, bool, error) {
	return coordination.Record{}, false, nil
}

func TestLaneMoveEntryPointsPreserveWorkerCompletionLane(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"targeted refresh", "kanban"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("entry", "example/repo#3", "Blocked")
			cfg := laneMutationTestConfig()
			backend, attempt := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, at)
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, at)
			state := newState(cfg)
			runCtx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: attempt, Generation: 1, stop: cancel}
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus}

			if entry == "targeted refresh" {
				moved := cloneIssue(issue)
				moved.State = "Rework"
				orch.applyTargetedReconcile(t.Context(), &state, connector.ReconcileTarget{}, connector.ReconcileResult{Found: true, Issue: moved}, at)
			} else {
				result := orch.applyOperatorMove(t.Context(), &state, OperatorMoveRequest{IssueID: issue.ID, FromState: issue.State, ToState: "Rework", WriteTracker: true}, at)
				if result.err != nil {
					t.Fatal(result.err)
				}
			}
			if _, claimed := state.Claimed[issue.ID]; !claimed {
				t.Fatal("lane move discarded the active worker claim")
			}
			running := state.Running[issue.ID]
			if running.CompletionLane != "Rework" || running.Issue.State != "Rework" || context.Cause(runCtx) != nil {
				t.Fatalf("worker lane=%q completion=%q cancellation=%v", running.Issue.State, running.CompletionLane, context.Cause(runCtx))
			}
		})
	}
}

type notifyingLaneLock struct {
	sync.Locker
	entered chan struct{}
}

func (l notifyingLaneLock) Lock() {
	l.entered <- struct{}{}
	l.Locker.Lock()
}

type notifyingLaneStore struct {
	store.LaneLedgerStore
	lock sync.Locker
}

func (s notifyingLaneStore) LaneWriteLock() sync.Locker {
	return s.lock
}

func TestLaneWritesSerializeAcrossOrchestratorEntryPoints(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	issue := laneRevocationIssue("serial", "example/repo#4", "Todo")
	cfg := laneMutationTestConfig()
	backend, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, at)
	lockEntered := make(chan struct{}, 2)
	ledger := notifyingLaneStore{LaneLedgerStore: backend, lock: notifyingLaneLock{Locker: backend.LaneWriteLock(), entered: lockEntered}}
	trackerEntered := make(chan string, 2)
	release := make(chan struct{}, 2)
	defer close(release)
	tracker := ledgerCheckingConnector{update: func(ctx context.Context, _, target string) error {
		trackerEntered <- target
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	first := newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, at)
	second := newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, at)
	first.laneLedger, second.laneLedger = ledger, ledger
	results := make(chan error, 2)
	go func() { results <- first.updateIssueState(t.Context(), nil, issue, "Rework", at, "test_transition") }()
	<-lockEntered
	if target := <-trackerEntered; target != "Rework" {
		t.Fatalf("first target=%q", target)
	}
	go func() {
		results <- second.updateIssueState(t.Context(), nil, issue, "In Progress", at.Add(time.Second), "test_transition")
	}()
	<-lockEntered
	write, result, err := backend.LatestLaneWrite(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
	if err != nil || write.To != "Rework" || result != "prepared" {
		t.Fatalf("second writer overtook first: write=%#v result=%s err=%v", write, result, err)
	}
	release <- struct{}{}
	if target := <-trackerEntered; target != "In Progress" {
		t.Fatalf("second target=%q", target)
	}
	release <- struct{}{}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestObservedLaneCompletionRetainsResultOnPersistenceFailure(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(strconv.FormatBool(failed), func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("completion", "example/repo#5", "Rework")
			attempts := &recordingWorkAttemptStore{}
			if failed {
				attempts.completionErrors = []error{errors.New("store unavailable")}
			}
			cfg := laneMutationTestConfig()
			orch := &Orchestrator{cfg: cfg, workAttempts: attempts, now: func() time.Time { return at }}
			state := newState(cfg)
			running := Running{Issue: issue, WorkAttemptID: 1, Generation: 1, CompletionLane: issue.State, CompletionOwnershipReleased: true}
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: at, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted}}
			orch.finishObservedLaneRun(t.Context(), &state, running, event)
			deferred, found := state.deferredCompletions[issue.ID]
			if found != failed {
				t.Fatalf("deferred=%t, want %t", found, failed)
			}
			if failed && (deferred.Result.FinalState != event.Result.FinalState || deferred.Running.CompletionLane != issue.State) {
				t.Fatalf("lost completed result or observed lane: %#v", deferred)
			}
			if len(state.Running) != 0 {
				t.Fatal("finished worker restored as live")
			}
		})
	}
}

func TestObservedLanePreTurnFailureRemainsInstanceOwned(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Todo", "Rework", "Blocked", "Done"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("startup", "example/repo#6", lane)
			tracker := &runningStateConnector{issues: []connector.Issue{issue}}
			attempts := &recordingWorkAttemptStore{}
			cfg := laneMutationTestConfig()
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			state := newState(cfg)
			running := Running{Issue: issue, WorkAttemptID: 1, CompletionLane: lane, DispatchSourceState: "Todo", DispatchTargetState: "In Progress"}
			orch.finishObservedLaneRun(t.Context(), &state, running, runpkg.Completion{IssueID: issue.ID, CompletedAt: at, Err: errors.New("backend failed before turn")})
			if !state.FailureBreaker.PreTurn || len(attempts.completions) != 1 || attempts.completions[0].ErrorClass != workAttemptErrorRunner {
				t.Fatalf("pre-turn failure lost instance attribution: breaker=%#v completions=%#v", state.FailureBreaker, attempts.completions)
			}
			if len(tracker.updates) != 0 || len(state.Retry) != 0 {
				t.Fatalf("pre-turn failure overwrote observed lane: writes=%v retry=%v", tracker.updates, state.Retry)
			}
		})
	}
}

func TestHumanMoveToMergingDoesNotChangeWorkerMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{runpkg.RunModeImplement, runpkg.RunModePlan, ""} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("mode", "example/repo#7", "In Progress")
			moved := cloneIssue(issue)
			moved.State = "Merging"
			moved.PullRequest = &connector.PullRequest{Number: 7, State: "OPEN", Draft: true}
			tracker := &runningStateConnector{issues: []connector.Issue{moved}}
			cfg := laneMutationTestConfig()
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			runCtx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			state.Running[issue.ID] = Running{Issue: issue, Mode: mode, stop: cancel}
			for index := range 2 {
				orch.reconcileRunningIssues(t.Context(), &state, at.Add(time.Duration(index)*time.Hour))
			}
			if context.Cause(runCtx) != nil || len(orch.pendingMergeRevocations) != 0 {
				t.Fatalf("lane move invoked merge-worker cancellation: cause=%v pending=%v", context.Cause(runCtx), orch.pendingMergeRevocations)
			}
			if running := state.Running[issue.ID]; running.Mode != mode || running.CompletionLane != "Merging" {
				t.Fatalf("worker mode=%q completion lane=%q", running.Mode, running.CompletionLane)
			}
		})
	}
}

func TestLaneAcknowledgementDoesNotHideLaterHumanReentry(t *testing.T) {
	t.Parallel()
	for _, peer := range []bool{false, true} {
		t.Run(strconv.FormatBool(peer), func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			issue := laneRevocationIssue("reentry", "example/repo#8", "Todo")
			cfg := laneMutationTestConfig()
			backend, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, at)
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, at)
			state := newState(cfg)
			if _, _, err := orch.observeLane(t.Context(), &state, issue, at); err != nil {
				t.Fatal(err)
			}
			var peerStore coordination.Store
			if peer {
				write := coordination.LaneWrite{InstanceIdentity: "peer", Issue: issue.ID, From: issue.State, To: "In Progress", Reason: "dispatch", FenceToken: 1, WrittenAt: at.Add(time.Second)}
				value, err := json.Marshal(map[string]map[string]coordination.LaneWrite{"writers": {"peer": write}})
				if err != nil {
					t.Fatal(err)
				}
				peerStore = lanePeerReader{value: value}
				orch.laneCoordination = peerStore
			} else if err := orch.updateIssueState(t.Context(), &state, issue, "In Progress", at.Add(time.Second), "dispatch"); err != nil {
				t.Fatal(err)
			}
			issue.State = "In Progress"
			if _, origin, err := orch.observeLane(t.Context(), &state, issue, at.Add(2*time.Second)); err != nil || origin.Origin != provenance.OriginDetent {
				t.Fatalf("first acknowledgement: origin=%s err=%v", origin.Origin, err)
			}
			issue.State = "Blocked"
			if _, _, err := orch.observeLane(t.Context(), &state, issue, at.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus}
			state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: 2}
			orch = newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, at)
			orch.laneCoordination = peerStore
			issue.State = "In Progress"
			for range 2 {
				if _, origin, err := orch.observeLane(t.Context(), &state, issue, at.Add(4*time.Second)); err != nil || origin.Origin != provenance.OriginHuman {
					t.Fatalf("human reentry: origin=%s err=%v", origin.Origin, err)
				}
			}
			if _, blocked := state.Blocked[issue.ID]; blocked || len(state.InstantFailures) != 0 {
				t.Fatal("human reentry did not clear failure memory")
			}
			timeline, err := backend.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
			want := 6
			if peer {
				want = 4
			}
			if err != nil || len(timeline.Events) != want {
				t.Fatalf("events=%d want=%d err=%v", len(timeline.Events), want, err)
			}
		})
	}
}
