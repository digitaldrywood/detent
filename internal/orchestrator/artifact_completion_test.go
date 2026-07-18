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
