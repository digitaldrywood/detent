package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestProgressCheckUsesReadOnlyBackendAndJSONBudget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, "")
	}
	output := `{"outcome":"reroute","step":"implement","reason":"` + strings.Repeat("evidence ", 100) + `","evidence":["failed-attempt"]}`
	backend := &progressPolicyBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{InputTokens: 100, OutputTokens: 50}}, {Type: AgentUpdateMessageDelta, ItemID: "final", Delta: output}}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: "codex", Model: "example-model", Default: true}}}}}, Workspace: progressWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: root}}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	execution := &progressUsageExecution{}
	decision, err := r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example-issue", IsolationPolicy: &isolation.Policy{Tier: isolation.NativeTrusted}}, Mode: RunModeImplement, Execution: execution}, progresscheck.Request{ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision", Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
	if err != nil || decision.Outcome != progresscheck.Reroute {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if len(execution.entries) != 1 || execution.entries[0].Input != 100 || execution.entries[0].Output != 50 || !execution.started {
		t.Fatalf("unaccounted progress turn: %+v", execution)
	}
	request := backend.request
	if request.MaxDuration != 3*time.Minute {
		t.Fatalf("progress deadline=%v", request.MaxDuration)
	}
	if !request.ReadOnly || !request.TextOnly || request.SupplementalTools || request.MaxTurns != 1 || request.Resume.ThreadID != "" || filepath.Dir(filepath.Dir(request.Workspace)) != workspace.WorkerScratchRoot(root) {
		t.Fatalf("progress request escaped read-only isolated turn: %+v", request)
	}
	if _, err := os.Stat(request.Workspace); !os.IsNotExist(err) {
		t.Fatalf("progress scratch retained: %v", err)
	}
	if !strings.Contains(request.Prompt, "failed-attempt") || !strings.Contains(request.Prompt, progresscheck.Instructions) {
		t.Fatal("backend did not receive evidence and decision contract")
	}
}

type progressPolicyBackend struct{ fakeCodexClient }

func (b *progressPolicyBackend) RunTurn(ctx context.Context, req AgentTurnRequest, observe AgentUpdateHandler) (AgentTurnResult, error) {
	if policy, ok := isolation.FromContext(ctx); !ok || policy.Tier != isolation.NativeTrusted {
		return AgentTurnResult{}, errors.New("runner claim isolation policy is unavailable")
	}
	return b.fakeCodexClient.RunTurn(ctx, req, observe)
}

type progressUsageExecution struct {
	recordingUsageExecution
	started    bool
	identity   tracker.NativeExecutionIdentity
	startError error
}

func (e *progressUsageExecution) Start(_ context.Context, identity tracker.NativeExecutionIdentity) error {
	if e.startError != nil {
		return e.startError
	}
	e.identity = identity
	e.started = true
	return nil
}

func (e *progressUsageExecution) RecordUsage(ctx context.Context, entry tracker.NativeUsage) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !e.started {
		return errors.New("execution has not started")
	}
	return e.recordingUsageExecution.RecordUsage(ctx, entry)
}

type stalledProgressBackend struct{ fakeCodexClient }

func (b *stalledProgressBackend) RunTurn(ctx context.Context, req AgentTurnRequest, observe AgentUpdateHandler) (AgentTurnResult, error) {
	b.request = req
	if err := observe(AgentUpdate{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{InputTokens: 10, OutputTokens: 900}}); err != nil {
		return AgentTurnResult{}, err
	}
	<-ctx.Done()
	return AgentTurnResult{}, ctx.Err()
}

func TestProgressCheckTimeoutAndAdmissionAccounting(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, "")
	}
	for _, test := range []struct {
		name         string
		rejected     bool
		agent        config.Agent
		codex        config.Codex
		wantDuration time.Duration
	}{
		{name: "timeout reports reasoning spend", wantDuration: 3 * time.Minute},
		{name: "capacity rejection starts no provider", rejected: true},
		{name: "configured turn limit", agent: config.Agent{MaxTurnDurationMS: 45000}, wantDuration: 45 * time.Second},
		{name: "backend normal turn limit", codex: config.Codex{TurnTimeoutMS: 90000}, wantDuration: 90 * time.Second},
		{name: "long backend turn is bounded", codex: config.Codex{TurnTimeoutMS: 600000}, wantDuration: 3 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				backend := &stalledProgressBackend{}
				execution := &progressUsageExecution{}
				if test.rejected {
					execution.startError = errors.New("provider capacity unavailable")
				}
				r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Agent: test.agent, Codex: test.codex, Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: "codex", Model: "", Default: true}}}}}, Workspace: progressWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: root}}}, AgentBackend: backend})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Mode: RunModePlan, Execution: execution}, progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
				if test.rejected {
					if !errors.Is(err, execution.startError) || backend.request.MaxDuration != 0 || len(execution.entries) != 0 {
						t.Fatalf("rejected check spent provider capacity: %v %+v", err, execution.entries)
					}
				} else if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != test.wantDuration || len(execution.entries) != 1 || execution.entries[0].Output != 900 || execution.entries[0].Model != "provider_default" || backend.request.Model != "" || execution.identity.Role != RolePlan {
					t.Fatalf("timeout/accounting: elapsed=%v err=%v usage=%+v identity=%+v", time.Since(start), err, execution.entries, execution.identity)
				}
			})
		})
	}
}

type progressWorkspace struct{ *fakeWorkspaceBackend }

func (w progressWorkspace) Existing(workspace.Issue) (workspace.Info, error) { return w.info, nil }

func TestProgressCheckMissingCheckoutUsesWorkerScratch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, "")
	}
	backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"progressing","reason":"Changed evidence","evidence":["failed"]}`}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Workspace: config.Workspace{Root: root}}}, Workspace: missingProgressWorkspace{&fakeWorkspaceBackend{}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Mode: RunModeImplement, Execution: &progressUsageExecution{}}, progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
	if err != nil {
		t.Fatal(err)
	}
	if !backend.request.ReadOnly || !backend.request.TextOnly || filepath.Dir(filepath.Dir(backend.request.Workspace)) != workspace.WorkerScratchRoot(root) {
		t.Fatalf("unconfined progress request: %+v", backend.request)
	}
	if _, err := os.Stat(backend.request.Workspace); !os.IsNotExist(err) {
		t.Fatalf("scratch retained: %v", err)
	}
}

type missingProgressWorkspace struct{ *fakeWorkspaceBackend }

func (w missingProgressWorkspace) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{}, workspace.ErrMissingWorkspace
}
