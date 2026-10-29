package runner

import (
	"context"
	"encoding/json"
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
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	output := `{"outcome":"reroute","step":"implement","diagnosis":"failed-attempt: TestExample asserts the repaired value but the implementation still returns the old value","instruction":"Update the code exercised by TestExample and rerun the assertion","reason":"` + strings.Repeat("evidence ", 100) + `","evidence":["failed-attempt"]}`
	backend := &progressPolicyBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{InputTokens: 100, OutputTokens: 50}}, {Type: AgentUpdateMessageDelta, ItemID: "final", Delta: output}}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: "codex", Model: "example-model", Default: true}}}}}, Workspace: progressWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: root}}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	execution := &progressReadExecution{}
	evidence := progresscheck.Request{ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision", Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}}
	beforeID, err := evidence.ID()
	if err != nil {
		t.Fatal(err)
	}
	decision, err := r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example-issue", IsolationPolicy: &isolation.Policy{Tier: isolation.NativeTrusted}}, Mode: RunModeImplement, Execution: execution}, evidence)
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
	if !request.ReadOnly || request.TextOnly || !request.SupplementalTools || request.MaxTurns != 8 || request.Resume.ThreadID != "" || request.Workspace != root {
		t.Fatalf("progress request escaped read-only isolated turn: %+v", request)
	}
	if _, err := os.Stat(request.TempDir); !os.IsNotExist(err) {
		t.Fatalf("progress scratch retained: %v", err)
	}
	if !strings.Contains(request.Prompt, "failed-attempt") || !strings.Contains(request.Prompt, progresscheck.Instructions) {
		t.Fatal("backend did not receive evidence and decision contract")
	}
	if !strings.Contains(request.Prompt, "TestExample: expected repaired value, got old value") || !backend.refusedWrite || execution.writes != 0 || execution.reads != 2 {
		t.Fatalf("investigation lost diagnostics or permitted writes: prompt=%s writes=%d reads=%d", request.Prompt, execution.writes, execution.reads)
	}
	afterID, err := evidence.ID()
	if err != nil || afterID != beforeID {
		t.Fatalf("investigation changed publication identity: %s -> %s, %v", beforeID, afterID, err)
	}
}

type progressPolicyBackend struct {
	fakeCodexClient
	refusedWrite bool
}

func (b *progressPolicyBackend) RunTurnWithTools(ctx context.Context, req AgentTurnRequest, tools []AgentTool, handler AgentToolHandler, observe AgentUpdateHandler) (AgentTurnResult, error) {
	for _, tool := range tools {
		if tool.Name == "attach_evidence" {
			return AgentTurnResult{}, errors.New("progress turn exposed a write tool")
		}
	}
	result, err := handler(ctx, AgentToolCall{Name: "attach_evidence", Arguments: json.RawMessage(`{}`)})
	b.refusedWrite = err != nil && !result.Success
	for _, arguments := range []json.RawMessage{
		json.RawMessage(`{"project_id":"another-project","reference":"example-issue","native_attempt_id":"failed-attempt"}`),
		json.RawMessage(`{"project_id":"example-project","reference":"another-issue","native_attempt_id":"failed-attempt"}`),
		json.RawMessage(`{"project_id":"example-project","reference":"example-issue","native_attempt_id":"outside-history"}`),
	} {
		if _, err := handler(ctx, AgentToolCall{Name: operatortool.WorkAttemptReceipt, Arguments: arguments}); err == nil {
			return AgentTurnResult{}, errors.New("progress turn read outside supplied history")
		}
	}
	_, err = handler(ctx, AgentToolCall{Name: operatortool.WorkAttemptReceipt, Arguments: json.RawMessage(`{"project_id":"example-project","reference":"example-issue","native_attempt_id":"failed-attempt"}`)})
	if err != nil {
		return AgentTurnResult{}, err
	}
	return b.RunTurn(ctx, req, observe)
}

type progressReadExecution struct {
	progressUsageExecution
	reads, writes int
}

func (e *progressReadExecution) AgentTools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{{Name: operatortool.WorkAttemptReceipt}, {Name: "attach_evidence"}}, func(_ context.Context, call AgentToolCall) (AgentToolResult, error) {
		if call.Name == "attach_evidence" {
			e.writes++
			return AgentToolResult{Success: true}, nil
		}
		e.reads++
		return AgentToolResult{Success: true, Content: `{"data":{"attempt":{"attempt_id":"failed-attempt","runtime":{"validation":{"command":"go test ./example","exit_code":1,"output":"TestExample: expected repaired value, got old value"}}}}}`}, nil
	}
}

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
	backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"progressing","reason":"Assertion repaired","diagnosis":"failed: TestExample returned the old value","convergence":"TestExample now passes on the repaired source","evidence":["failed"]}`}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Workspace: config.Workspace{Root: root}}}, Workspace: missingProgressWorkspace{&fakeWorkspaceBackend{info: workspace.Info{Path: root}}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Mode: RunModeImplement, Execution: &progressUsageExecution{}}, progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
	if err != nil {
		t.Fatal(err)
	}
	if !backend.request.ReadOnly || backend.request.TextOnly || backend.request.Workspace != root {
		t.Fatalf("unconfined progress request: %+v", backend.request)
	}
	if _, err := os.Stat(backend.request.TempDir); !os.IsNotExist(err) {
		t.Fatalf("scratch retained: %v", err)
	}
}

type missingProgressWorkspace struct{ *fakeWorkspaceBackend }

func (w missingProgressWorkspace) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{}, workspace.ErrMissingWorkspace
}

func TestProgressCheckRestoresPinnedSource(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	for _, test := range []struct {
		name, mismatch string
	}{
		{name: "fresh runner restores Hub bundle"},
		{name: "moved source refused", mismatch: "head"},
		{name: "moved base refused", mismatch: "base"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			repository := "https://github.com/example-org/example-project"
			runRunnerGit(t, source, "remote", "add", "origin", repository)
			fresh := filepath.Join(t.TempDir(), "source")
			runRunnerGit(t, source, "clone", "--no-local", source, fresh)
			runRunnerGit(t, fresh, "remote", "set-url", "origin", repository)
			runRunnerGit(t, fresh, "config", "url."+source+".insteadOf", repository)
			attemptDiffFile(t, source, "assertion.txt", "failing assertion and exercised code\n")
			runRunnerGit(t, source, "add", "assertion.txt")
			runRunnerGit(t, source, "commit", "-m", "retain failing source")
			head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			capture, err := workspace.CaptureChangeSource(t.Context(), source, base, head)
			if err != nil {
				t.Fatal(err)
			}
			version := tracker.ChangeVersion{ID: "version_example", ChangeID: "change_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: repository, BaseSHA: base, HeadSHA: head, Source: &capture.Source}}
			execution := &progressSourceExecution{source: workspace.ChangeSource{Version: version, Bundle: capture.Bundle}}
			evidence := progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", SourceSHA: head, BaseSHA: base, Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}}
			if test.mismatch == "head" {
				evidence.SourceSHA = base
			}
			if test.mismatch == "base" {
				evidence.BaseSHA = head
			}
			local, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: fresh, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			backend := &progressInspectBackend{head: head}
			r, err := NewRunner(Dependencies{Workspace: local, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example", Identifier: "example#1"}, Mode: RunModeImplement, Execution: execution}, evidence)
			if test.mismatch != "" {
				if err == nil || backend.called {
					t.Fatalf("mismatched source reached provider: %v", err)
				}
			} else if err != nil || !backend.called {
				t.Fatalf("fresh runner could not investigate retained source: %v", err)
			}
		})
	}
}

type progressSourceExecution struct {
	progressUsageExecution
	source workspace.ChangeSource
}

func (*progressSourceExecution) SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
}

func (e *progressSourceExecution) RecoverChangeSource(context.Context) (workspace.ChangeSource, error) {
	return e.source, nil
}

type progressInspectBackend struct {
	head   string
	called bool
}

func (b *progressInspectBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.called = true
	content, err := os.ReadFile(filepath.Join(req.Workspace, "assertion.txt"))
	if err != nil || string(content) != "failing assertion and exercised code\n" || !req.ReadOnly || req.TextOnly || !req.IsolatedTools || !strings.Contains(req.Prompt, b.head) {
		return AgentTurnResult{}, errors.New("pinned source unavailable to read-only investigation")
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"reroute","step":"implement","reason":"Repair the failing assertion","diagnosis":"failed: assertion.txt retains the failing assertion","instruction":"Change the implementation exercised by assertion.txt and run the matching validation command","evidence":["failed"]}`})
}
