package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestSessionBrakeReleasesSlotRecordsCauseAndParks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		landing      *runpkg.NativeLanding
		productive   bool
		resumable    bool
		diff         DiffStats
		activeStates []string
		reworkState  string
		wantState    string
		finalState   string
		wantTerminal store.WorkAttemptTerminalState
	}{
		{name: "dirty source does not reset", resumable: true, diff: DiffStats{HeadSHA: "committed-head", TreeSHA: "after", FilesChanged: 1}, wantState: "Rework", finalState: runpkg.FinalStateSessionDurationExceeded, wantTerminal: store.WorkAttemptTerminalTimedOut},
		{name: "missing custody does not reset", resumable: true, diff: DiffStats{HeadSHA: "committed-head", TreeSHA: "after", RecoveryStateExpected: true}, wantState: "Rework", finalState: runpkg.FinalStateSessionDurationExceeded, wantTerminal: store.WorkAttemptTerminalTimedOut},
		{name: "rejected publication does not reset", resumable: true, landing: &runpkg.NativeLanding{Refusal: "publication refused"}, diff: DiffStats{HeadSHA: "committed-head", TreeSHA: "after"}, wantState: "Rework", finalState: runpkg.FinalStateSessionDurationExceeded, wantTerminal: store.WorkAttemptTerminalTimedOut},
		{name: "verified committed progress", productive: true, resumable: true, diff: DiffStats{HeadSHA: "committed-head", TreeSHA: "after"}, wantState: "Rework", finalState: runpkg.FinalStateSessionDurationExceeded, wantTerminal: store.WorkAttemptTerminalTimedOut},
		{
			name:         "no work product returns to todo",
			wantState:    "Todo",
			finalState:   runpkg.FinalStateSessionDurationExceeded,
			wantTerminal: store.WorkAttemptTerminalTimedOut,
		},
		{
			name:      "workspace progress moves to rework",
			resumable: true,
			diff: DiffStats{
				FilesChanged:    2,
				AddedLines:      12,
				UnpushedCommits: 1,
				Fingerprint:     "workspace-progress",
				Status:          "changed",
			},
			wantState:    "Rework",
			finalState:   runpkg.FinalStateSessionDurationExceeded,
			wantTerminal: store.WorkAttemptTerminalTimedOut,
		},
		{
			name:         "workspace progress uses configured rework state",
			resumable:    true,
			activeStates: []string{"Todo", "In Progress", "Production Rework"},
			reworkState:  "Production Rework",
			wantState:    "production rework",
			finalState:   runpkg.FinalStateSessionDurationExceeded,
			wantTerminal: store.WorkAttemptTerminalTimedOut,
		},
		{
			name:         "workspace progress falls back to todo when rework is inactive",
			resumable:    true,
			activeStates: []string{"Todo", "In Progress"},
			wantState:    "Todo",
			finalState:   runpkg.FinalStateSessionDurationExceeded,
			wantTerminal: store.WorkAttemptTerminalTimedOut,
		},
		{
			name:         "memory ceiling returns to todo without breaker strike",
			wantState:    "Todo",
			finalState:   runpkg.FinalStateMemoryCeilingExceeded,
			wantTerminal: store.WorkAttemptTerminalMemoryCeiling,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			startedAt := time.Date(2026, 7, 30, 17, 0, 0, 0, time.UTC)
			completedAt := startedAt.Add(2 * time.Hour)
			issue := connector.Issue{
				ID:               "issue-1572-" + strings.ReplaceAll(tt.name, " ", "-"),
				Identifier:       "digitaldrywood/detent#1572",
				Title:            "Session brake",
				State:            "In Progress",
				AssignedToWorker: true,
			}
			var events []fakehub.Event
			tracker := fakehub.New(fakehub.Config{
				Issues:    []connector.Issue{issue},
				Stateful:  true,
				Now:       func() time.Time { return completedAt },
				EventSink: func(event fakehub.Event) { events = append(events, event) },
			})
			brake := &runpkg.SessionBrakeError{
				Reason:           runpkg.SessionBrakeReasonDuration,
				CauseFingerprint: "cause-fingerprint-1572",
				Limit:            2 * time.Hour,
				MaxTurns:         20,
				Elapsed:          2 * time.Hour,
				Turns:            7,
				Tokens:           456789,
				LastProgressAt:   startedAt.Add(45 * time.Minute),
				Resumable:        tt.resumable,
			}
			switch tt.finalState {
			case runpkg.FinalStateMemoryCeilingExceeded:
				brake.Reason = runpkg.SessionBrakeReasonMemory
				brake.RSSBytes = 9 << 30
				brake.RSSCeilingBytes = 8 << 30
				brake.CauseFingerprint = "memory-ceiling-1572"
			}
			runner := sessionBrakeCompletionRunner{
				result: RunResult{
					FinalState:  tt.finalState,
					TurnStarted: true,
					Tokens: TokenTotals{
						TotalTokens:    brake.Tokens,
						RuntimeSeconds: brake.Elapsed.Seconds(),
					},
					DiffStats:     tt.diff,
					NativeLanding: tt.landing,
				},
				err: brake,
			}
			project := scheduler.ProjectCandidate{ID: "detent"}
			dispatchGate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
			activeStates := tt.activeStates
			if len(activeStates) == 0 {
				activeStates = []string{"Todo", "In Progress", "Rework"}
			}
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1,
				Project:             project,
				ActiveStates:        activeStates,
				ObservedStates:      []string{"Blocked"},
				TerminalStates:      []string{"Done", "Cancelled"},
				AutoPromote: AutoPromoteConfig{
					ReworkState: tt.reworkState,
				},
			})
			supervisor, err := runpkg.NewSupervisor(runner, runpkg.SupervisorConfig{
				Now: func() time.Time { return completedAt },
			})
			if err != nil {
				t.Fatalf("NewSupervisor() error = %v", err)
			}
			attempts := &recordingWorkAttemptStore{}
			var logs bytes.Buffer
			orch := &Orchestrator{
				cfg:                cfg,
				connector:          tracker,
				supervisor:         supervisor,
				workAttempts:       attempts,
				globalDispatchGate: dispatchGate,
				runResults:         make(chan runpkg.Completion, 1),
				runUpdates:         make(chan runUpdate, 1),
				logger:             slog.New(slog.NewTextHandler(&logs, nil)),
			}
			db, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "retry.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			orch.workflowMetrics = db
			state := newState(cfg)

			if !orch.dispatchIssue(t.Context(), &state, issue, 3, startedAt, "") {
				t.Fatal("dispatchIssue() = false, want true")
			}
			var completion runpkg.Completion
			select {
			case completion = <-orch.runResults:
			case <-t.Context().Done():
				t.Fatal("timed out waiting for session brake completion")
			}
			if completion.Retryable {
				t.Fatal("session brake completion is retryable")
			}
			scheduling := &nativeCompletionScheduling{hubSchedulingSource: &hubSchedulingSource{}}
			orch.scheduling = scheduling
			running := state.Running[issue.ID].withProgress()
			running.progress = nil
			running.CompletionSourceStart.Captured = true
			running.CompletionSourceStart.Fingerprint.WorkspaceTree = "before"
			state.Running[issue.ID] = running
			orch.handleRunResult(t.Context(), &state, completion)
			wantAttempt, upper := 4, 80*time.Second
			if tt.productive {
				wantAttempt, upper = 1, 10*time.Second
			}
			assertCompletionMachineRetry(t, orch, state, issue.ID, completedAt, wantAttempt, upper)
			if scheduling.releases != 1 {
				t.Fatalf("lease releases = %d, want 1", scheduling.releases)
			}

			if _, ok := state.Running[issue.ID]; ok {
				t.Fatalf("Running[%q] present after session brake", issue.ID)
			}
			if _, ok := state.Retry[issue.ID]; !ok {
				t.Fatalf("Retry[%q] missing after session brake", issue.ID)
			}
			if _, ok := state.Blocked[issue.ID]; ok {
				t.Fatalf("Blocked[%q] present after session brake", issue.ID)
			}
			if retry := state.Retry[issue.ID]; retry.Issue.State != tt.wantState {
				t.Fatalf("retry state = %q, want %q", retry.Issue.State, tt.wantState)
			}
			if availableSlots(&state) != 1 {
				t.Fatalf("availableSlots() = %d, want released local slot", availableSlots(&state))
			}
			if _, ok, err := dispatchGate.TryAcquire(
				t.Context(),
				project,
				scheduler.SlotRequest{State: tt.wantState},
				completedAt,
			); err != nil || !ok {
				t.Fatalf("TryAcquire() after session brake = %v, %v, want released global slot", ok, err)
			}

			if len(attempts.completions) != 1 {
				t.Fatalf("work attempt completions = %#v, want one", attempts.completions)
			}
			if attempts.completions[0].TerminalState != tt.wantTerminal {
				t.Fatalf("terminal state = %q, want %q", attempts.completions[0].TerminalState, tt.wantTerminal)
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
				t.Fatalf("unmarshal work attempt metadata: %v", err)
			}
			sessionBrake, ok := metadata[sessionBrakeMetadataKey].(map[string]any)
			if !ok || sessionBrake["cause_fingerprint"] != brake.CauseFingerprint {
				t.Fatalf("session brake metadata = %#v, want cause fingerprint", metadata)
			}
			if tt.finalState == runpkg.FinalStateMemoryCeilingExceeded {
				if sessionBrake["rss_bytes"] != float64(brake.RSSBytes) || sessionBrake["rss_ceiling_bytes"] != float64(brake.RSSCeilingBytes) {
					t.Fatalf("session brake metadata = %#v, want RSS evidence", sessionBrake)
				}
			}

			var stateUpdated bool
			var comment string
			for _, event := range events {
				switch event.Kind {
				case fakehub.EventKindStateUpdate:
					stateUpdated = event.State == tt.wantState
				case fakehub.EventKindComment:
					comment = event.Body
				}
			}
			if !stateUpdated {
				t.Fatalf("events = %#v, want state update to %q", events, tt.wantState)
			}
			if !strings.Contains(comment, "cause_fingerprint: "+brake.CauseFingerprint) ||
				!strings.Contains(comment, "parked_state: "+tt.wantState) {
				t.Fatalf("session brake comment = %q, want fingerprint and parked state", comment)
			}
			if strings.Contains(logs.String(), "session brake state transition failed") {
				t.Fatalf("logs = %q, want configured parking state transition", logs.String())
			}
			for _, want := range []string{
				"level=WARN",
				"msg=session_brake_tripped",
				"elapsed=2h0m0s",
				"turns=7",
				"tokens=456789",
			} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("logs = %q, want %q", logs.String(), want)
				}
			}
			attempt := wantAttempt
			for cycle, changed := range []bool{true, true, false, false, true} {
				running.Attempt = attempt
				running.Issue = issue
				running.DiffStats = tt.diff
				repeated := completion
				repeated.Result.DiffStats = tt.diff
				if tt.productive && !changed {
					repeated.Result.DiffStats.TreeSHA = "before"
				}
				want := attempt + 1
				if tt.productive && changed {
					want = 1
				}
				state.Claimed[issue.ID] = Claimed{Issue: issue}
				beforeReleases := scheduling.releases
				if !orch.handleSessionBrake(t.Context(), &state, repeated, running) {
					t.Fatal("session brake not handled")
				}
				assertCompletionMachineRetry(t, orch, state, issue.ID, completedAt, want, min(10*time.Second<<(want-1), cfg.MaxRetryBackoff))
				if scheduling.releases != beforeReleases+1 {
					t.Fatalf("cycle %d releases = %d, want %d", cycle, scheduling.releases, beforeReleases+1)
				}
				attempt = want
			}

		})
	}
}

func TestSessionProgressProbeTracksLatestWorkpadContent(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{
		ID:         "issue-workpad-probe",
		Identifier: "digitaldrywood/detent#1572",
		State:      "In Progress",
		Comments: []connector.IssueComment{
			{Body: "## Codex Workpad\n\ninitial"},
		},
	}
	tracker := fakehub.New(fakehub.Config{Issues: []connector.Issue{issue}, Stateful: true})
	orch := &Orchestrator{connector: tracker}
	probe := orch.sessionProgressProbe(issue)
	if probe == nil {
		t.Fatal("sessionProgressProbe() = nil")
	}

	before, err := probe(t.Context())
	if err != nil {
		t.Fatalf("initial probe error = %v", err)
	}
	if err := tracker.CreateComment(t.Context(), issue.ID, "## Codex Workpad\n\nupdated"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	after, err := probe(t.Context())
	if err != nil {
		t.Fatalf("updated probe error = %v", err)
	}
	if before == "" || after == "" || before == after {
		t.Fatalf("workpad fingerprints = %q then %q, want change", before, after)
	}
}

type sessionBrakeCompletionRunner struct {
	result RunResult
	err    error
}

func (r sessionBrakeCompletionRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return r.result, r.err
}

var _ Runner = sessionBrakeCompletionRunner{}
