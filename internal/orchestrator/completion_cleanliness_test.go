package orchestrator

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestEvaluateCompletionCleanliness(t *testing.T) {
	t.Parallel()

	previousRejected := store.WorkAttempt{
		TerminalState: store.WorkAttemptTerminalSuccess,
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			completionCleanlinessMetadataKey: completionCleanlinessRecord{Outcome: completionCleanlinessRejected},
		}),
	}
	resolvedIssue := func(resolution string) connector.Issue {
		return connector.Issue{WorkpadSignal: &workpad.Signal{
			Source: workpad.SourceStructured,
			Status: workpad.StatusComplete,
			Fields: map[string]string{
				workpad.FieldCompletionAttempt:               "2147",
				workpad.FieldCompletionGeneration:            "7",
				workpad.FieldCompletionCleanlinessResolution: resolution,
			},
		}}
	}
	tests := []struct {
		name           string
		evidence       DiffStats
		history        []store.WorkAttempt
		historyErr     error
		issue          connector.Issue
		wantOutcome    string
		wantResolution string
		wantRejected   int
		wantBlock      bool
		withoutLane    bool
		wantWarning    bool
	}{
		{
			name:           "clean tree",
			evidence:       DiffStats{Status: "clean", HeadSHA: "head"},
			wantOutcome:    completionCleanlinessAccepted,
			wantResolution: completionCleanlinessClean,
		},
		{
			name:           "tracked modifications",
			evidence:       DiffStats{FilesChanged: 1, AddedLines: 4, RemovedLines: 2, TrackedPaths: []string{"internal/worker.go"}, Status: "changed"},
			wantOutcome:    completionCleanlinessRejected,
			wantResolution: "required",
			wantRejected:   1,
		},
		{
			name:           "untracked files",
			evidence:       DiffStats{FilesChanged: 1, AddedLines: 8, UntrackedPaths: []string{"debug.log"}, Status: "changed"},
			wantOutcome:    completionCleanlinessRejected,
			wantResolution: "required",
			wantRejected:   1,
		},
		{
			name: "unpushed commits without pull request",
			evidence: DiffStats{
				UnpushedCommits:    1,
				UnpushedCommitRefs: []string{"abc123 fix: retain work"},
				HeadSHA:            "abc123",
				Status:             "clean",
			},
			wantOutcome:    completionCleanlinessRejected,
			wantResolution: "required",
			wantRejected:   1,
		},
		{
			name:           "committed after rejection",
			evidence:       DiffStats{Status: "clean", HeadSHA: "resolved-head"},
			history:        []store.WorkAttempt{previousRejected},
			issue:          resolvedIssue(completionCleanlinessCommitted),
			wantOutcome:    completionCleanlinessAccepted,
			wantResolution: completionCleanlinessCommitted,
		},
		{
			name:           "discarded after rejection",
			evidence:       DiffStats{Status: "clean", HeadSHA: "original-head"},
			history:        []store.WorkAttempt{previousRejected},
			issue:          resolvedIssue(completionCleanlinessDiscarded),
			wantOutcome:    completionCleanlinessAccepted,
			wantResolution: completionCleanlinessDiscarded,
		},
		{
			name:         "clean retry without explicit resolution escalates",
			evidence:     DiffStats{Status: "clean", HeadSHA: "resolved-head"},
			history:      []store.WorkAttempt{previousRejected},
			wantOutcome:  completionCleanlinessEscalated,
			wantRejected: 2,
			wantBlock:    true,
		},
		{
			name: "recovery evidence unavailable",
			evidence: DiffStats{
				Status:                "clean",
				RecoveryStateExpected: true,
			},
			wantOutcome:  completionCleanlinessRejected,
			wantRejected: 1,
		},
		{
			name:         "audit history unavailable escalates",
			evidence:     DiffStats{Status: "clean", HeadSHA: "head"},
			historyErr:   errors.New("history unavailable"),
			wantOutcome:  completionCleanlinessEscalated,
			wantRejected: 1,
			wantBlock:    true,
			wantWarning:  true,
		},
		{
			name:           "repeated dirty completion escalates",
			evidence:       DiffStats{FilesChanged: 1, UntrackedPaths: []string{"debug.log"}, Status: "changed"},
			history:        []store.WorkAttempt{previousRejected},
			wantOutcome:    completionCleanlinessEscalated,
			wantResolution: "required",
			wantRejected:   2,
			wantBlock:      true,
		},
		{
			name:     "intentional remainder escalates",
			evidence: DiffStats{FilesChanged: 1, TrackedPaths: []string{"fixture.patch"}, Status: "changed"},
			issue: connector.Issue{WorkpadSignal: &workpad.Signal{
				Source:      workpad.SourceStructured,
				Status:      workpad.StatusBlocked,
				HumanAction: "preserve fixture.patch for operator inspection",
			}},
			wantOutcome:    completionCleanlinessEscalated,
			wantResolution: completionCleanlinessIntentionallyLeft,
			wantRejected:   1,
			wantBlock:      true,
		},
		{
			name:     "intentional remainder resolves prior rejection without another completion declaration",
			evidence: DiffStats{FilesChanged: 1, TrackedPaths: []string{"fixture.patch"}, Status: "changed"},
			history:  []store.WorkAttempt{previousRejected},
			issue: connector.Issue{WorkpadSignal: &workpad.Signal{
				Source:      workpad.SourceStructured,
				Status:      workpad.StatusBlocked,
				HumanAction: "preserve fixture.patch for operator inspection",
			}},
			wantOutcome:    completionCleanlinessEscalated,
			wantResolution: completionCleanlinessIntentionallyLeft,
			wantRejected:   2,
			wantBlock:      true,
			withoutLane:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attempts := &implementProgressAttemptStore{history: tt.history, historyErr: tt.historyErr}
			orch := &Orchestrator{
				cfg:          normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}}),
				workAttempts: attempts,
			}
			issue := tt.issue
			issue.ID = "issue-2147"
			issue.Identifier = "digitaldrywood/detent#2147"
			completionLane := "In Progress"
			if tt.withoutLane {
				completionLane = ""
			}
			decision := orch.evaluateCompletionCleanliness(t.Context(), Running{
				Issue:          issue,
				Mode:           runpkg.RunModeImplement,
				WorkAttemptID:  2147,
				Generation:     7,
				CompletionLane: completionLane,
			}, issue, tt.evidence)

			if !decision.Attempted || decision.Outcome != tt.wantOutcome || decision.Resolution != tt.wantResolution || decision.ConsecutiveRejections != tt.wantRejected || decision.Block != tt.wantBlock {
				t.Fatalf("evaluateCompletionCleanliness() = %#v", decision)
			}
			if (decision.Warning != "") != tt.wantWarning {
				t.Fatalf("evaluateCompletionCleanliness().Warning = %q", decision.Warning)
			}
			metadata := completionCleanlinessMetadata(decision)
			persisted := store.WorkAttempt{WorkerMetadataJSON: marshalWorkAttemptJSON(metadata)}
			record, ok := completionCleanlinessRecordFromAttempt(persisted)
			if !ok || record.Outcome != tt.wantOutcome || record.Resolution != tt.wantResolution || record.ConsecutiveRejections != tt.wantRejected {
				t.Fatalf("persisted completion cleanliness = %#v, ok=%t", record, ok)
			}
			if tt.wantResolution == completionCleanlinessCommitted || tt.wantResolution == completionCleanlinessDiscarded {
				if record.Declaration != tt.wantResolution {
					t.Fatalf("persisted declaration = %q, want %q", record.Declaration, tt.wantResolution)
				}
			}
			if len(tt.evidence.UntrackedPaths) > 0 && len(record.Evidence.UntrackedPaths) == 0 {
				t.Fatalf("persisted evidence = %#v, want untracked paths", record.Evidence)
			}
		})
	}
}

func TestHandleRunResultRejectsDirtyCompletionAndEscalates(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	previousRejected := store.WorkAttempt{
		TerminalState: store.WorkAttemptTerminalSuccess,
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			completionCleanlinessMetadataKey: completionCleanlinessRecord{Outcome: completionCleanlinessRejected},
		}),
	}
	tests := []struct {
		name         string
		history      []store.WorkAttempt
		wantTerminal store.WorkAttemptTerminalState
		wantRetry    bool
		wantBlocked  bool
		intentional  bool
	}{
		{name: "first rejection returns to agent", wantTerminal: store.WorkAttemptTerminalSuccess, wantRetry: true},
		{name: "second rejection parks", history: []store.WorkAttempt{previousRejected}, wantTerminal: store.WorkAttemptTerminalNoProgress, wantBlocked: true},
		{name: "intentional remainder parks", history: []store.WorkAttempt{previousRejected}, wantTerminal: store.WorkAttemptTerminalNoProgress, wantBlocked: true, intentional: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := connector.Issue{ID: "issue-2147", Identifier: "digitaldrywood/detent#2147", State: "In Progress"}
			completionLane := "In Progress"
			if tt.intentional {
				completionLane = ""
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: preserve debug.log for operator inspection\n```",
				}}
			}
			tracker := &implementProgressConnector{refreshed: issue}
			attempts := &implementProgressAttemptStore{history: tt.history}
			cfg := normalizeConfig(Config{
				Project:                scheduler.ProjectCandidate{ID: "detent"},
				ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
				ObservedStates:         []string{"Blocked"},
				TerminalStates:         []string{"Done", "Cancelled"},
				ContinuationRetryDelay: time.Minute,
			})
			orch := &Orchestrator{
				cfg:          cfg,
				connector:    tracker,
				workAttempts: attempts,
				logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			state := newState(cfg)
			state.Running[issue.ID] = Running{
				Issue:          issue,
				Attempt:        1,
				WorkAttemptID:  2147,
				Generation:     7,
				Mode:           runpkg.RunModeImplement,
				CompletionLane: completionLane,
				StartedAt:      base.Add(-time.Minute),
			}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: base.Add(-time.Minute)}
			diff := DiffStats{
				FilesChanged:   2,
				AddedLines:     9,
				RemovedLines:   3,
				TrackedPaths:   []string{"internal/worker.go"},
				UntrackedPaths: []string{"debug.log"},
				Fingerprint:    "dirty-fingerprint",
				Status:         "changed",
			}

			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				CompletedAt: base,
				Request:     runpkg.RunRequest{Issue: issue, Mode: runpkg.RunModeImplement, WorkAttemptID: 2147, Generation: 7},
				Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, DiffStats: diff},
			})

			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != tt.wantTerminal {
				t.Fatalf("work attempt completions = %#v, want %q", attempts.completions, tt.wantTerminal)
			}
			var metadata struct {
				CompletionCleanliness completionCleanlinessRecord `json:"completion_cleanliness"`
			}
			if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
				t.Fatalf("decode work attempt metadata: %v", err)
			}
			if metadata.CompletionCleanliness.Outcome == "" || len(metadata.CompletionCleanliness.Evidence.UntrackedPaths) != 1 {
				t.Fatalf("completion cleanliness metadata = %#v", metadata.CompletionCleanliness)
			}
			_, retrying := state.Retry[issue.ID]
			_, blocked := state.Blocked[issue.ID]
			if retrying != tt.wantRetry || blocked != tt.wantBlocked {
				t.Fatalf("state = retry %t blocked %t, want retry %t blocked %t", retrying, blocked, tt.wantRetry, tt.wantBlocked)
			}
			if len(tracker.comments) == 0 {
				t.Fatal("expected a completion rejection or escalation comment")
			}
			comment := tracker.comments[len(tracker.comments)-1].body
			for _, want := range []string{"2 files, +9/-3", "internal/worker.go", "debug.log"} {
				if !strings.Contains(comment, want) {
					t.Fatalf("comment missing %q:\n%s", want, comment)
				}
			}
			if !tt.intentional {
				for _, want := range []string{"completion_cleanliness_resolution", "committed", "discarded", "under `fields:`"} {
					if !strings.Contains(comment, want) {
						t.Fatalf("comment missing %q:\n%s", want, comment)
					}
				}
			} else if !strings.Contains(comment, "preserve debug.log for operator inspection") {
				t.Fatalf("comment missing intentional-left statement:\n%s", comment)
			}
		})
	}
}
