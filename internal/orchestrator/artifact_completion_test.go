package orchestrator

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestHandleRunResultAppliesArtifactGateWorkpadField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		workpadStatus    string
		workpadFields    string
		workpadBeforeRun bool
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
			workpadBeforeRun: true,
			wantStatus:       "recut",
			wantLogFragments: []string{"artifact gate Workpad field update skipped", "artifact gate completion left status field unchanged"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 7, 18, 18, 0, 0, 0, time.UTC)
			workpadAt := now.Add(-10 * time.Second)
			if tt.workpadBeforeRun {
				workpadAt = now.Add(-2 * time.Minute)
			}
			issue := connector.NewIssue()
			issue.ID = "issue-artifact-completion"
			issue.Identifier = "digitaldrywood/video#42"
			issue.Title = "Render artifact"
			issue.State = "Production"
			issue.Fields = map[string]string{"render_status": "recut"}
			issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
			tracker := &autoPromoteTickConnector{
				stateIssues: []connector.Issue{issue},
				issueComments: map[string][]connector.IssueComment{
					issue.ID: {{
						Body:      "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: " + tt.workpadStatus + "\n" + tt.workpadFields + "blockers: []\nhuman_action: null\n```",
						URL:       "https://github.test/comment/42",
						CreatedAt: &workpadAt,
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
				Issue:     cloneIssue(issue),
				Attempt:   1,
				StartedAt: now.Add(-time.Minute),
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
