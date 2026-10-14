package orchestrator

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestSoakHardSessionTokenCeilingInvariant(t *testing.T) {
	if os.Getenv("DETENT_RUN_SOAK_TESTS") != "1" {
		t.Skip("set DETENT_RUN_SOAK_TESTS=1 to run local orchestrator soak tests")
	}

	const (
		simulatedAttempts = 1_000
		maxSessions       = 3
		tokensPerSession  = int64(500_000)
		maxTokens         = int64(maxSessions) * tokensPerSession
	)
	base := time.Date(2026, 7, 11, 16, 0, 0, 0, time.UTC)
	issue := implementProgressIssueWithoutPR()
	issue.AssignedToWorker = true
	tracker := &implementProgressConnector{}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:        scheduler.ProjectCandidate{ID: "soak"},
		AutoPromote:    AutoPromoteConfig{NoProgressLimit: maxSessions},
		ActiveStates:   []string{"Todo", "In Progress", "Rework"},
		ObservedStates: []string{"Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
	state := newState(cfg)
	sessions := 0
	var tokens int64

	for index := range simulatedAttempts {
		completedAt := base.Add(time.Duration(index+1) * time.Minute)
		diff := DiffStats{FilesChanged: 1, AddedLines: 1, Status: "changed"}
		setCompatibilityDiffFingerprint(&diff, "same-staged-diff")
		state.Running[issue.ID] = Running{
			Issue:         issue,
			Attempt:       index + 1,
			WorkAttemptID: int64(index + 1),
			Mode:          runpkg.RunModeImplement,
			StartedAt:     completedAt.Add(-time.Minute),
			DiffStats:     diff,
		}
		state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: completedAt.Add(-time.Minute)}
		delete(state.Retry, issue.ID)
		delete(state.Completed, issue.ID)

		orch.handleRunResult(context.Background(), &state, runpkg.Completion{
			IssueID:     issue.ID,
			CompletedAt: completedAt,
			Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
			Result: runpkg.RunResult{
				FinalState: runpkg.FinalStateCompleted,
				DiffStats:  diff,
				Tokens:     TokenTotals{InputTokens: tokensPerSession - 1, OutputTokens: 1, TotalTokens: tokensPerSession},
			},
		})
		sessions++
		tokens += tokensPerSession
		completion := attempts.completions[len(attempts.completions)-1]
		attempts.history = append([]store.WorkAttempt{{
			TerminalState:      completion.TerminalState,
			WorkerMetadataJSON: completion.WorkerMetadataJSON,
			CompletedAt:        completion.CompletedAt,
		}}, attempts.history...)
		if _, blocked := state.Blocked[issue.ID]; blocked {
			break
		}
	}

	if sessions > maxSessions {
		t.Fatalf("sessions = %d, ceiling %d", sessions, maxSessions)
	}
	if tokens > maxTokens {
		t.Fatalf("tokens = %d, ceiling %d", tokens, maxTokens)
	}
	if _, blocked := state.Blocked[issue.ID]; !blocked {
		t.Fatalf("issue did not reach Blocked after %d simulated attempts", simulatedAttempts)
	}
}

func setCompatibilityDiffFingerprint(diff *DiffStats, fingerprint string) {
	field := reflect.ValueOf(diff).Elem().FieldByName("Fingerprint")
	if field.IsValid() && field.CanSet() && field.Kind() == reflect.String {
		field.SetString(fingerprint)
	}
}
