package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestSessionBrakeReleasesSlotRecordsCauseAndParks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		resumable  bool
		diff       DiffStats
		wantState  string
		finalState string
	}{
		{
			name:       "no work product returns to todo",
			wantState:  "Todo",
			finalState: runpkg.FinalStateSessionDurationExceeded,
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
			wantState:  "Rework",
			finalState: runpkg.FinalStateNoProgress,
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
			var events []memory.Event
			tracker := memory.New(memory.Config{
				Issues:    []connector.Issue{issue},
				Stateful:  true,
				Now:       func() time.Time { return completedAt },
				EventSink: func(event memory.Event) { events = append(events, event) },
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
			if tt.finalState == runpkg.FinalStateNoProgress {
				brake.Reason = runpkg.SessionBrakeReasonNoProgress
			}
			runner := sessionBrakeCompletionRunner{
				result: RunResult{
					FinalState:  tt.finalState,
					TurnStarted: true,
					Tokens: TokenTotals{
						TotalTokens:    brake.Tokens,
						RuntimeSeconds: brake.Elapsed.Seconds(),
					},
					DiffStats: tt.diff,
				},
				err: brake,
			}
			project := scheduler.ProjectCandidate{ID: "detent", Weight: 1}
			dispatchGate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1,
				Project:             project,
				ActiveStates:        []string{"Todo", "In Progress", "Rework"},
				ObservedStates:      []string{"Blocked"},
				TerminalStates:      []string{"Done", "Cancelled"},
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
			state := newState(cfg)

			if !orch.dispatchIssue(t.Context(), &state, issue, 1, startedAt, "") {
				t.Fatal("dispatchIssue() = false, want true")
			}
			var completion runpkg.Completion
			select {
			case completion = <-orch.runResults:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for session brake completion")
			}
			if completion.Retryable {
				t.Fatal("session brake completion is retryable")
			}
			orch.handleRunResult(t.Context(), &state, completion)

			if _, ok := state.Running[issue.ID]; ok {
				t.Fatalf("Running[%q] present after session brake", issue.ID)
			}
			if _, ok := state.Retry[issue.ID]; ok {
				t.Fatalf("Retry[%q] present after session brake", issue.ID)
			}
			if _, ok := state.Blocked[issue.ID]; ok {
				t.Fatalf("Blocked[%q] present after session brake", issue.ID)
			}
			completed, ok := state.Completed[issue.ID]
			if !ok || completed.Issue.State != tt.wantState {
				t.Fatalf("Completed[%q] = %#v, want parked state %q", issue.ID, completed, tt.wantState)
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
			var metadata map[string]any
			if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
				t.Fatalf("unmarshal work attempt metadata: %v", err)
			}
			sessionBrake, ok := metadata[sessionBrakeMetadataKey].(map[string]any)
			if !ok || sessionBrake["cause_fingerprint"] != brake.CauseFingerprint {
				t.Fatalf("session brake metadata = %#v, want cause fingerprint", metadata)
			}

			var stateUpdated bool
			var comment string
			for _, event := range events {
				switch event.Kind {
				case memory.EventKindStateUpdate:
					stateUpdated = event.State == tt.wantState
				case memory.EventKindComment:
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
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
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
