package orchestrator

import (
	"encoding/json"
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
	for _, tt := range []struct {
		name     string
		output   string
		question string
		kind     string
	}{
		{"assigned edits", "May I make the coordinated scheduling, connector, CI, test, and migration-documentation changes for #2346?\nNo implementation or tests have run.", "May I make the coordinated scheduling, connector, CI, test, and migration-documentation changes for #2346?", "implementation_permission"},
		{"explicit gate", "May I deploy these changes to production?", "May I deploy these changes to production?", "approval_or_input"},
		{"structured input", "## Codex Workpad\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Choose the storage architecture\n```", "Choose the storage architecture", "approval_or_input"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := connector.Issue{ID: "issue-2415", Identifier: "digitaldrywood/detent#2415", State: "In Progress"}
			tracker := &implementProgressConnector{refreshed: issue}
			attempts := &implementProgressAttemptStore{}
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"Todo", "In Progress", "Rework"}, ObservedStates: []string{"Blocked"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 5112, Generation: 169, SessionID: "session-5898", Mode: runpkg.RunModeImplement, StartedAt: time.Now()}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: time.Now(), Request: runpkg.RunRequest{WorkAttemptID: 5112, Generation: 169}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, FinalMessage: tt.output, DiffStats: DiffStats{Status: "clean", HeadSHA: "53c6b1c"}}})
			if len(attempts.completions) != 1 {
				t.Fatalf("completions = %d", len(attempts.completions))
			}
			completion := attempts.completions[0]
			if completion.ErrorClass != "permission_wait" || !strings.Contains(completion.ErrorMessage, tt.question) {
				t.Fatalf("completion lost question: %#v", completion)
			}
			var metadata struct {
				PermissionWait struct {
					Kind          string
					WorkAttemptID int64  `json:"work_attempt_id"`
					SessionID     string `json:"session_id"`
				} `json:"permission_wait"`
			}
			if err := json.Unmarshal([]byte(completion.WorkerMetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.PermissionWait.Kind != tt.kind || metadata.PermissionWait.WorkAttemptID != 5112 || metadata.PermissionWait.SessionID != "session-5898" {
				t.Fatalf("handoff = %#v", metadata.PermissionWait)
			}
			blocked, ok := state.Blocked[issue.ID]
			if !ok || blocked.Recovery.Owner != blockedRecoveryOwnerHuman || !strings.Contains(blocked.RecoveryRemedy, tt.question) {
				t.Fatalf("blocked = %#v", blocked)
			}
			for _, elapsed := range []time.Duration{time.Minute, 24 * time.Hour} {
				if orch.recoverCauseBlockedIssue(t.Context(), &state, blocked.Issue, time.Now().Add(elapsed)) {
					t.Fatal("unchanged question recovered automatically")
				}
			}
			if !strings.Contains(blocked.Recovery.AttemptError, "terminal_") || blocked.Recovery.WorkAttemptID != 5112 {
				t.Fatalf("durable recovery lost provenance: %#v", blocked.Recovery)
			}
			if _, ok := state.Retry[issue.ID]; ok {
				t.Fatal("permission wait scheduled a retry")
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
