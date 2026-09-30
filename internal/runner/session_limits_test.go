package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type sessionLimitsBackend struct {
	fakeCodexClient
	duringTurn func(AgentTurnRequest, AgentUpdateHandler) error
}

func (b *sessionLimitsBackend) RunTurn(_ context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, b.duringTurn(req, update)
}

func TestRunnerSelectedSessionLimits(t *testing.T) {
	for _, mode := range []string{"worker", "validator", "triage"} {
		for _, stop := range []string{"duration", "tokens"} {
			for _, explicit := range []bool{false, true} {
				name := mode + "/" + stop + "/inherit"
				if explicit {
					name = mode + "/" + stop + "/override"
				}
				t.Run(name, func(t *testing.T) {
					cfg := config.Config{Agent: config.Agent{MaxSessionDurationMS: 60000, MaxSessionTokens: 100, MaxTurnDurationMS: 30000, MaxTurns: 7, NoProgressTimeoutMS: 45000}}
					cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
					level := config.ModelSelectionDefaults{}
					wantDuration, wantTokens := 60000, int64(100)
					if explicit {
						wantDuration, wantTokens = 120000, 200
						level.MaxSessionDurationMS, level.MaxSessionTokens = new(wantDuration), new(wantTokens)
					}
					cfg.Agents.ModelSelection.Levels = map[string]config.ModelSelectionDefaults{"complex": level}
					// Validators ignore issue-wide complexity by default; exercise the existing stage override.
					cfg.Agents.ModelSelection.Stages = map[string]config.ModelSelectionStage{RoleValidator: {Level: new("complex")}}
					issue := connector.Issue{ID: "limits", Identifier: "detent#2597", Labels: []string{"complexity:complex"}}
					duration := &controlledDurationLimit{}
					backend := &sessionLimitsBackend{fakeCodexClient: fakeCodexClient{models: selectionCatalog()}}
					sessions := &fakeSessionStore{sessionID: 2597}
					runner, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Work"}, Workspace: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, AgentBackend: backend, Store: sessions, sessionLimit: duration.Context, SecurityAuditRoot: t.TempDir()})
					if err != nil {
						t.Fatal(err)
					}
					backend.duringTurn = func(req AgentTurnRequest, update AgentUpdateHandler) error {
						wantTurnDuration, wantTurns := 30*time.Second, 7
						if mode == "triage" {
							wantTurnDuration, wantTurns = 2*time.Minute, 1
						}
						if req.MaxDuration != wantTurnDuration || req.MaxTurns != wantTurns {
							t.Fatalf("turn guards changed: %+v", req)
						}
						if duration.duration != time.Duration(wantDuration)*time.Millisecond {
							t.Fatalf("duration = %s", duration.duration)
						}
						if err := update(AgentUpdate{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{TotalTokens: 10}}); err != nil {
							t.Fatal(err)
						}
						// Simulate a label refresh and policy reload after the first usage update.
						issue.Labels[0] = "complexity:very-complex"
						cfg.Agent.MaxSessionTokens = 9999
						cfg.Agent.MaxSessionDurationMS = 999999
						cfg.Agents.ModelSelection.Levels["complex"] = config.ModelSelectionDefaults{MaxSessionDurationMS: new(999999), MaxSessionTokens: new(int64(9999))}
						if err := runner.UpdateWorkflowChecked(config.Workflow{Config: cfg}); err != nil {
							t.Fatal(err)
						}
						if duration.duration != time.Duration(wantDuration)*time.Millisecond {
							t.Fatal("running duration changed")
						}
						if stop == "duration" {
							duration.Expire()
							return context.Canceled
						}
						return update(AgentUpdate{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{TotalTokens: wantTokens + 1}})
					}
					if mode != "validator" {
						runMode := RunModeImplement
						if mode == "triage" {
							runMode = RunModeTriage
						}
						_, err = runner.Run(t.Context(), RunRequest{Issue: issue, Mode: runMode, Attempt: 4, WorkAttemptID: 5602})
						if sessions.started.WorkAttemptID != 5602 {
							t.Fatalf("attempt changed: %+v", sessions.started)
						}
					} else {
						_, err = runner.Validate(t.Context(), testValidatorRequest(issue))
					}
					if stop == "duration" {
						if !errors.Is(err, ErrSessionDurationExceeded) {
							t.Fatalf("error = %v", err)
						}
					} else {
						var ceiling *SessionTokenCeilingError
						if !errors.As(err, &ceiling) || ceiling.CeilingTokens != wantTokens || ceiling.TotalTokens != wantTokens+1 {
							t.Fatalf("token limit error = %v", err)
						}
						if sessions.finished.TotalTokens != wantTokens+1 {
							t.Fatalf("usage reset: %+v", sessions.finished)
						}
					}
					if sessions.started.RuntimeIdentity.Selection.Level != "complex" {
						t.Fatalf("selected level changed: %+v", sessions.started.RuntimeIdentity.Selection)
					}
				})
			}
		}
	}
}

func TestResumedSelectionKeepsSessionLevel(t *testing.T) {
	for _, label := range []string{"complexity:complex", "complexity:very-complex"} {
		t.Run(label, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
			backend := &fakeCodexClient{models: selectionCatalog()}
			selected := resolveAgentSelection(t.Context(), connector.Issue{}, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
			identity := configuredRuntimeIdentity(RouteSelection{}, config.AgentBackend{}, RoleCode, selected.Model, time.Now())
			identity.Selection = selected.Selection
			req := RunRequest{Issue: connector.Issue{Labels: []string{label}}, Attempt: 4, WorkAttemptID: 5602, RetryMode: RetryModeResume, ResumeState: store.AgentResumeState{ProviderThreadID: "thread", RuntimeIdentity: identity}, sessionTokenOffset: 80, sessionTurnOffset: 3}
			got := resolveRequestAgentSelection(t.Context(), req, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
			if got.Err != nil || got.Selection.Level != "normal" {
				t.Fatalf("resumed selection = %+v", got)
			}
			if req.Attempt != 4 || req.WorkAttemptID != 5602 || req.sessionTokenOffset != 80 || req.sessionTurnOffset != 3 {
				t.Fatal("resume accounting changed")
			}
		})
	}
}

func TestMergeSelectedSessionDurationPreservesTurnLimit(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		sessionMS, turnMS, wantMS int
	}{
		{"disabled session preserves turn", 0, 30000, 30000},
		{"shorter session caps turn", 10000, 30000, 10000},
		{"longer session preserves turn", 60000, 30000, 30000},
		{"session caps unlimited turn", 60000, 0, 60000},
		{"both unlimited", 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Agent: config.Agent{MaxSessionDurationMS: 90000, MaxTurnDurationMS: tt.turnMS}, Agents: config.Agents{ModelSelection: config.ModelSelection{
				Preset: new("sol_first"),
				Levels: map[string]config.ModelSelectionDefaults{"complex": {MaxSessionDurationMS: new(tt.sessionMS)}},
				Stages: map[string]config.ModelSelectionStage{RoleMerge: {Level: new("complex")}},
			}}}
			backend := &fakeCodexClient{models: selectionCatalog()}
			runner, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &fakeMergeWorkspaceBackend{fakeWorkspaceBackend: fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, prepareResult: workspace.MergePrepareResult{Status: workspace.MergePrepareStatusConflict}}, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "merge-limits", Identifier: "detent#2597", State: "Merging"}, Mode: RunModeMerge})
			if err != nil {
				t.Fatal(err)
			}
			if backend.calls != 1 || backend.request.MaxDuration != time.Duration(tt.wantMS)*time.Millisecond {
				t.Fatalf("calls=%d turn limit=%s, want %dms", backend.calls, backend.request.MaxDuration, tt.wantMS)
			}
		})
	}
}
