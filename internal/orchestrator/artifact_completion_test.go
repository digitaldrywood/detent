package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestHandleRunResultAppliesArtifactGateWorkpadField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		workpadStatus    string
		workpadFields    string
		workpadUnchanged bool
		wantSetFields    []autoPromoteTickSetField
		wantStatus       string
		wantLogFragments []string
	}{
		{
			name:          "applies configured wait status",
			workpadStatus: "complete",
			workpadFields: "fields:\n  render_status: pending_review\n",
			wantSetFields: []autoPromoteTickSetField{{
				issueID: "issue-artifact-completion",
				field:   "render_status",
				value:   "pending_review",
			}},
			wantStatus: "pending_review",
		},
		{
			name:             "rejects unconfigured status",
			workpadStatus:    "complete",
			workpadFields:    "fields:\n  render_status: surprise\n",
			wantStatus:       "recut",
			wantLogFragments: []string{"artifact gate Workpad field update rejected", "artifact gate completion left status field unchanged"},
		},
		{
			name:             "warns when successful completion omits field update",
			workpadStatus:    "complete",
			wantStatus:       "recut",
			wantLogFragments: []string{"artifact gate completion left status field unchanged"},
		},
		{
			name:             "rejects field without completion declaration",
			workpadStatus:    "in_progress",
			workpadFields:    "fields:\n  render_status: pending_review\n",
			wantStatus:       "recut",
			wantLogFragments: []string{"artifact gate Workpad field update rejected", "artifact gate completion left status field unchanged"},
		},
		{
			name:             "skips field from stale completion declaration",
			workpadStatus:    "complete",
			workpadFields:    "fields:\n  render_status: pending_review\n",
			workpadUnchanged: true,
			wantStatus:       "recut",
			wantLogFragments: []string{"artifact gate Workpad field update skipped", "artifact gate completion left status field unchanged"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 7, 18, 18, 0, 0, 0, time.UTC)
			issue := connector.NewIssue()
			issue.ID = "issue-artifact-completion"
			issue.Identifier = "digitaldrywood/video#42"
			issue.Title = "Render artifact"
			issue.State = "Production"
			issue.Fields = map[string]string{"render_status": "recut"}
			issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
			workpadBody := "## Codex Workpad\n\n### Validation\n\nCurrent attempt.\n\n```detent-status\nschema: 1\nstatus: " + tt.workpadStatus + "\n" + tt.workpadFields + "blockers: []\nhuman_action: null\n```"
			dispatchWorkpadBody := "## Codex Workpad\n\n### Validation\n\nPrior attempt.\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
			if tt.workpadUnchanged {
				dispatchWorkpadBody = "## Codex Workpad\n\n### Validation\n\nPrior attempt.\n\n```detent-status\nschema: 1\nstatus: " + tt.workpadStatus + "\n" + tt.workpadFields + "blockers: []\nhuman_action: null\n```"
			}
			tracker := &autoPromoteTickConnector{
				stateIssues: []connector.Issue{issue},
				issueComments: map[string][]connector.IssueComment{
					issue.ID: {{
						Body: workpadBody,
						URL:  "https://github.test/comment/42",
					}},
				},
			}
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"Todo", "Production", "Rework"},
				ObservedStates: []string{"Backlog", "Review", "Blocked"},
				TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
				AutoPromote: AutoPromoteConfig{
					Enabled:       true,
					SourceState:   "Review",
					PassState:     "Ready for Pickup",
					ReworkState:   "Rework",
					GateWaitState: autoPromoteGateWaitSource,
					Gate: gate.Config{
						Kind: gate.KindArtifact,
						Artifact: gate.ArtifactConfig{
							StatusField:    "render_status",
							PassStatuses:   []string{"approved"},
							WaitStatuses:   []string{"pending_review"},
							ReworkStatuses: []string{"recut"},
						},
					},
				},
			})
			var logs strings.Builder
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(&logs, nil)),
			}
			state := newState(cfg)
			state.Running[issue.ID] = Running{
				Issue:               cloneIssue(issue),
				Attempt:             1,
				DispatchWorkpadHash: artifactGateWorkpadStatusHash([]connector.IssueComment{{Body: dispatchWorkpadBody}}),
				DispatchWorkpadRead: true,
				StartedAt:           now.Add(-time.Minute),
			}

			orch.handleRunResult(context.Background(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				CompletedAt: now,
				Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
			})

			if !reflect.DeepEqual(tracker.setFields, tt.wantSetFields) {
				t.Fatalf("set fields = %#v, want %#v", tracker.setFields, tt.wantSetFields)
			}
			completed, ok := state.Completed[issue.ID]
			if !ok {
				t.Fatalf("Completed[%q] missing", issue.ID)
			}
			if got := artifactStatusFromIssue(completed.Issue, "render_status"); got != tt.wantStatus {
				t.Fatalf("completed render_status = %q, want %q", got, tt.wantStatus)
			}
			for _, fragment := range tt.wantLogFragments {
				if !strings.Contains(logs.String(), fragment) {
					t.Fatalf("logs %q missing %q", logs.String(), fragment)
				}
			}
		})
	}
}

func TestHandleRunResultParksThirdUnchangedArtifactGateSuccess(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	issue := connector.NewIssue()
	issue.ID = "issue-artifact-convergence"
	issue.Identifier = "digitaldrywood/video#47"
	issue.Title = "Render storyboard"
	issue.State = "Rework"
	issue.Fields = map[string]string{"render_status": "recut"}
	issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			SourceState:   "Review",
			PassState:     "Ready for Pickup",
			ReworkState:   "Rework",
			GateWaitState: autoPromoteGateWaitSource,
			Gate:          artifactCompletionTestGate(),
		},
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	attempts := &recordingWorkAttemptStore{history: []store.WorkAttempt{
		{
			ProjectID:          cfg.Project.ID,
			IssueID:            issue.ID,
			Identifier:         issue.Identifier,
			WorkerType:         "agent",
			TerminalState:      store.WorkAttemptTerminalNoProgress,
			CompletedAt:        now.Add(-time.Minute),
			WorkerMetadataJSON: `{"artifact_gate_convergence":{"status_field":"render_status","dispatch_status":"recut","completion_status":"recut","unchanged":true,"consecutive_unchanged":2,"limit":3}}`,
		},
		{
			ProjectID:          cfg.Project.ID,
			IssueID:            issue.ID,
			Identifier:         issue.Identifier,
			WorkerType:         "agent",
			TerminalState:      store.WorkAttemptTerminalNoProgress,
			CompletedAt:        now.Add(-2 * time.Minute),
			WorkerMetadataJSON: `{"artifact_gate_convergence":{"status_field":"render_status","dispatch_status":"recut","completion_status":"recut","unchanged":true,"consecutive_unchanged":1,"limit":3}}`,
		},
	}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:         cloneIssue(issue),
		Attempt:       3,
		WorkAttemptID: 47,
		StartedAt:     now.Add(-40 * time.Second),
		DiffStats:     DiffStats{Status: "clean"},
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Blocked"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if blocked, ok := state.Blocked[issue.ID]; !ok || blocked.Issue.State != "Blocked" {
		t.Fatalf("Blocked[%q] = %#v, want parked Blocked issue", issue.ID, blocked)
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present after convergence breaker", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after convergence breaker", issue.ID)
	}
	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress {
		t.Fatalf("completions = %#v, want one durable no-progress worker success", attempts.completions)
	}
	if !strings.Contains(attempts.completions[0].WorkerMetadataJSON, `"tripped":true`) {
		t.Fatalf("worker metadata = %q, want durable convergence trip", attempts.completions[0].WorkerMetadataJSON)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "stopped redispatching") {
		t.Fatalf("comments = %#v, want convergence breaker handoff", tracker.comments)
	}
	for _, fragment := range []string{"artifact gate convergence breaker tripped", "consecutive_unchanged=3"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing %q", logs.String(), fragment)
		}
	}
}

func TestConsecutiveArtifactGateConvergenceAttempts(t *testing.T) {
	t.Parallel()

	current := artifactGateConvergenceRecord{
		StatusField:      "render_status",
		DispatchStatus:   "recut",
		CompletionStatus: "recut",
		Unchanged:        true,
		Limit:            artifactGateConvergenceLimit,
	}
	tests := []struct {
		name     string
		attempts []store.WorkAttempt
		want     int
	}{
		{
			name: "counts matching successes",
			attempts: []store.WorkAttempt{
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalSuccess, current),
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalSuccess, current),
			},
			want: 2,
		},
		{
			name: "counts worker successes reclassified as no progress",
			attempts: []store.WorkAttempt{
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalNoProgress, current),
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalNoProgress, current),
			},
			want: 2,
		},
		{
			name: "field progress resets count",
			attempts: []store.WorkAttempt{
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalSuccess, artifactGateConvergenceRecord{
					StatusField:      "render_status",
					DispatchStatus:   "recut",
					CompletionStatus: "pending_review",
					Limit:            artifactGateConvergenceLimit,
				}),
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalSuccess, current),
			},
		},
		{
			name: "different unchanged value resets count",
			attempts: []store.WorkAttempt{
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalSuccess, artifactGateConvergenceRecord{
					StatusField:          "render_status",
					DispatchStatus:       "pending_review",
					CompletionStatus:     "pending_review",
					Unchanged:            true,
					ConsecutiveUnchanged: 1,
					Limit:                artifactGateConvergenceLimit,
				}),
			},
		},
		{
			name: "failure breaks successful sequence",
			attempts: []store.WorkAttempt{
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalFailure, current),
				artifactGateConvergenceAttempt(t, store.WorkAttemptTerminalSuccess, current),
			},
		},
		{
			name: "legacy success breaks recorded sequence",
			attempts: []store.WorkAttempt{{
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: `{}`,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := consecutiveArtifactGateConvergenceAttempts(tt.attempts, current); got != tt.want {
				t.Fatalf("consecutiveArtifactGateConvergenceAttempts() = %d, want %d", got, tt.want)
			}
		})
	}
}

func artifactGateConvergenceAttempt(
	t *testing.T,
	terminalState store.WorkAttemptTerminalState,
	record artifactGateConvergenceRecord,
) store.WorkAttempt {
	t.Helper()
	metadata, err := json.Marshal(artifactGateConvergenceMetadata(record))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return store.WorkAttempt{TerminalState: terminalState, WorkerMetadataJSON: string(metadata)}
}

func TestArtifactGateCompletionFieldUpdate(t *testing.T) {
	t.Parallel()

	cfg := gate.ArtifactConfig{
		StatusField:    "render_status",
		PassStatuses:   []string{"approved"},
		WaitStatuses:   []string{"pending_review"},
		ReworkStatuses: []string{"recut"},
	}
	tests := []struct {
		name      string
		fields    map[string]string
		wantField string
		wantValue string
		wantErr   bool
	}{
		{name: "pass status", fields: map[string]string{"render_status": "approved"}, wantField: "render_status", wantValue: "approved"},
		{name: "wait status", fields: map[string]string{"render_status": "pending_review"}, wantField: "render_status", wantValue: "pending_review"},
		{name: "rework status", fields: map[string]string{"render_status": "recut"}, wantField: "render_status", wantValue: "recut"},
		{name: "case insensitive field and status", fields: map[string]string{"Render_Status": " APPROVED "}, wantField: "render_status", wantValue: "APPROVED"},
		{name: "unknown status", fields: map[string]string{"render_status": "surprise"}, wantErr: true},
		{name: "wrong field", fields: map[string]string{"validation_status": "approved"}, wantErr: true},
		{name: "multiple fields", fields: map[string]string{"render_status": "approved", "owner": "worker"}, wantErr: true},
		{name: "missing field", fields: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			field, value, err := artifactGateCompletionFieldUpdate(tt.fields, cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("artifactGateCompletionFieldUpdate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if field != tt.wantField || value != tt.wantValue {
				t.Fatalf("artifactGateCompletionFieldUpdate() = (%q, %q), want (%q, %q)", field, value, tt.wantField, tt.wantValue)
			}
		})
	}
}

func TestArtifactGateWorkpadStatusHashUsesOnlyStructuredBlock(t *testing.T) {
	t.Parallel()

	block := "```detent-status\nschema: 1\nstatus: complete\nfields:\n  render_status: pending_review\nblockers: []\nhuman_action: null\n```"
	prior := artifactGateWorkpadStatusHash([]connector.IssueComment{{Body: "## Codex Workpad\n\nPrior prose.\n\n" + block}})
	updatedProse := artifactGateWorkpadStatusHash([]connector.IssueComment{{Body: "## Codex Workpad\n\nUpdated prose only.\n\n" + block}})
	updatedBlock := artifactGateWorkpadStatusHash([]connector.IssueComment{{Body: "## Codex Workpad\n\nUpdated prose.\n\n```detent-status\nschema: 1\nstatus: complete\nfields:\n  render_status: approved\nblockers: []\nhuman_action: null\n```"}})

	if prior == "" {
		t.Fatal("artifactGateWorkpadStatusHash() is empty")
	}
	if updatedProse != prior {
		t.Fatalf("prose-only update hash = %q, want %q", updatedProse, prior)
	}
	if updatedBlock == prior {
		t.Fatalf("structured block update hash = %q, want different from %q", updatedBlock, prior)
	}
}

func TestArtifactGateDispatchWorkpadSnapshot(t *testing.T) {
	t.Parallel()

	issue := connector.NewIssue()
	issue.ID = "issue-artifact-snapshot"
	issue.Identifier = "digitaldrywood/video#43"
	comment := connector.IssueComment{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"}
	tracker := &autoPromoteTickConnector{issueComments: map[string][]connector.IssueComment{issue.ID: {comment}}}
	orch := &Orchestrator{
		cfg:       normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindArtifact}}}),
		connector: tracker,
	}

	hash, ok := orch.artifactGateDispatchWorkpadSnapshot(context.Background(), issue)

	if !ok {
		t.Fatal("artifactGateDispatchWorkpadSnapshot() ok = false")
	}
	if want := artifactGateWorkpadStatusHash([]connector.IssueComment{comment}); hash != want {
		t.Fatalf("artifactGateDispatchWorkpadSnapshot() hash = %q, want %q", hash, want)
	}
	if !reflect.DeepEqual(tracker.fetchComments, []string{issue.ID}) {
		t.Fatalf("FetchIssueComments() issues = %#v, want %#v", tracker.fetchComments, []string{issue.ID})
	}
}
