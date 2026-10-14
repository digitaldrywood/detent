package orchestrator

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestHandleRunResultPermissionWait(t *testing.T) {
	t.Parallel()
	complete := "## Codex Workpad\n```detent-status\nschema: 1\nstatus: complete\nfields:\n  completion_work_attempt_id: \"5112\"\n  completion_generation: \"169\"\nblockers: []\nhuman_action: null\n```"
	blocked := "## Codex Workpad\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Choose the storage architecture\n```"
	for _, tt := range []struct {
		name        string
		output      string
		workpad     string
		commentErr  error
		refreshErr  error
		noUsage     bool
		wantBlocked bool
		wantReview  bool
	}{
		{name: "complete canonical workpad with final question", output: "May I merge?", workpad: complete, wantReview: true},
		{name: "complete canonical workpad with contradictory final status", output: blocked, workpad: complete, wantReview: true, noUsage: true},
		{name: "canonical human action with final question", output: "May I deploy these changes to production?", workpad: blocked, wantBlocked: true},
		{name: "canonical human action with final completion", output: complete, workpad: blocked, wantBlocked: true},
		{name: "unfinished canonical workpad with final completion", output: complete, workpad: strings.Replace(complete, "status: complete", "status: in_progress", 1)},
		{name: "missing current evidence with final completion", output: complete, refreshErr: errors.New("tracker unavailable")},
		{name: "unreadable current workpad with final completion", output: complete, workpad: strings.Replace(complete, "status: complete", "status: in_progress", 1), commentErr: errors.New("comments unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := implementProgressIssue("ready-head")
			if tt.commentErr != nil {
				issue.Comments = []connector.IssueComment{{Body: strings.Replace(complete, "status: complete", "status: in_progress", 1)}}
			}
			current := cloneIssue(issue)
			current.Comments = []connector.IssueComment{{Body: tt.workpad}}
			tracker := &implementProgressConnector{refreshed: current, hydrated: current, refreshErr: tt.refreshErr, commentErr: tt.commentErr}
			attempts := &implementProgressAttemptStore{}
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"Todo", "In Progress", "Rework"}, ObservedStates: []string{"Blocked"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			state.TokenTotals.TotalTokens = 10
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 5112, Generation: 169, SessionID: "session-5898", Mode: runpkg.RunModeImplement, Tokens: TokenTotals{TotalTokens: 42}, DiffStats: DiffStats{HeadSHA: "previous"}, StartedAt: time.Now()}
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			tokens := TokenTotals{TotalTokens: 42}
			if tt.noUsage {
				tokens = TokenTotals{}
			}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: time.Now(), Request: runpkg.RunRequest{WorkAttemptID: 5112, Generation: 169}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, FinalMessage: tt.output, Tokens: tokens, DiffStats: DiffStats{Status: "clean", HeadSHA: "53c6b1c"}}})
			if tt.refreshErr != nil {
				if _, deferred := state.deferredCompletions[issue.ID]; !deferred || len(attempts.completions) != 0 || len(tracker.updates) != 0 || !state.Retry[issue.ID].CompletionDeferred {
					t.Fatalf("unavailable tracker completion was not deferred: %#v", attempts.completions)
				}
				return
			}
			if len(attempts.completions) != 1 || attempts.completions[0].ErrorClass == permissionWaitReason || strings.Contains(attempts.completions[0].WorkerMetadataJSON, `"permission_wait"`) {
				t.Fatalf("final prose classified completion: %#v", attempts.completions)
			}
			if state.TokenTotals.TotalTokens != 52 || state.DiffStats[issue.ID].HeadSHA != "53c6b1c" {
				t.Fatalf("completion telemetry lost: tokens=%#v diff=%#v", state.TokenTotals, state.DiffStats[issue.ID])
			}
			if _, claimed := state.Claimed[issue.ID]; claimed {
				t.Fatal("completion retained claim")
			}
			if got, ok := state.Blocked[issue.ID]; ok != tt.wantBlocked || ok && got.Reason != workpadBlockedUnactionedReason {
				t.Fatalf("canonical blocked outcome = %#v, present = %t, want %t", got, ok, tt.wantBlocked)
			}
			if got := len(tracker.updates) > 0 && tracker.updates[len(tracker.updates)-1].state == "Human Review"; got != tt.wantReview {
				t.Fatalf("review transition = %t, want %t; updates = %#v", got, tt.wantReview, tracker.updates)
			}
		})
	}
}

func TestTerminalPermissionWait(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, output string
		want         bool
	}{
		{"ordinary no progress", "No implementation or tests have run.", false},
		{"completed", "Implemented the fix and ran tests.", false},
		{"quoted example", "> May I deploy to production?", false},
		{"code example", "```text\nMay I deploy to production?\n```", false},
		{"tilde example", "~~~text\nMay I deploy to production?\n~~~", false},
		{"nested shorter fence", "````text\n```\nMay I deploy to production?\n````", false},
		{"mixed fence", "~~~text\n```\nMay I deploy to production?\n~~~", false},
		{"after fence", "~~~text\nexample\n~~~\nMay I deploy to production?", true},
		{"implementation", "Can I edit the assigned files?", true},
		{"architecture", "Do you approve the storage architecture?", true},
		{"access", "May I access the production database?", true},
		{"withdrawal", "Do you authorize withdrawing the release?", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got := terminalPermissionWait(tt.output)
			if got != tt.want {
				t.Fatalf("wait = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestPermissionWaitIgnoresNonterminalOutput(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		result runpkg.RunResult
	}{
		{"earlier question", runpkg.RunResult{Output: "May I deploy to production?", FinalMessage: "The authorized fix is complete."}},
		{"failed run", runpkg.RunResult{FinalState: runpkg.FinalStateFailed, FinalMessage: "May I deploy to production?"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orch := &Orchestrator{}
			if orch.handlePermissionWaitCompletion(t.Context(), nil, runpkg.Completion{Result: tt.result}, Running{}) {
				t.Fatal("unexpected wait")
			}
		})
	}
}
