package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Prompt: "## Progress Review\nProject-specific assertion investigation.\n## Other\nDo not include this section.", Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: "codex", Model: "example-model", Default: true}}}}}, Workspace: progressWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: root}}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	execution := &progressReadExecution{}
	evidence := progresscheck.Request{ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision", SourceSHA: "head", BaseSHA: "base", Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}}
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
	if request.MaxDuration <= 0 || request.MaxDuration > 3*time.Minute || request.TurnTimeout != request.MaxDuration {
		t.Fatalf("progress deadline=%v", request.MaxDuration)
	}
	if !request.ReadOnly || request.TextOnly || !request.SupplementalTools || request.MaxTurns != 8 || request.Resume.ThreadID != "" || request.Workspace != root {
		t.Fatalf("progress request escaped read-only isolated turn: %+v", request)
	}
	if _, err := os.Stat(request.TempDir); !os.IsNotExist(err) {
		t.Fatalf("progress scratch retained: %v", err)
	}
	if !strings.Contains(request.Prompt, "Project-specific assertion investigation.") || strings.Contains(request.Prompt, "Do not include this section.") || strings.Contains(request.Prompt, progresscheck.DefaultGuidance) {
		t.Fatal("project progress review did not reach prompt exclusively")
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
				r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: config.Config{Agent: test.agent, Codex: test.codex, Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: "codex", Model: "", Default: true}}}}}, Workspace: progressWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: root}}}, AgentBackend: backend})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Mode: RunModePlan, Execution: execution}, progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", SourceSHA: "head", BaseSHA: "base", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
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

func TestProgressSetupLeavesTimeForDecision(t *testing.T) {
	for _, test := range []struct {
		name                    string
		setup, limit, remaining time.Duration
		wantDeadline            bool
	}{
		{name: "slow setup", setup: 160 * time.Second, remaining: 20 * time.Second},
		{name: "short configured limit", setup: 160 * time.Second, limit: 8 * time.Second, remaining: 8 * time.Second},
		{name: "setup consumes deadline", setup: 3 * time.Minute, wantDeadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			execution := &progressSetupExecution{advance: func() { now = now.Add(test.setup) }}
			backend := &progressDeadlineBackend{t: t, now: &now, remaining: test.remaining}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Now: func() time.Time { return now }, Workflow: config.Workflow{Config: config.Config{Agent: config.Agent{MaxTurnDurationMS: int(test.limit / time.Millisecond)}}}, Workspace: progressWorkspace{&fakeWorkspaceBackend{}}, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Mode: RunModeImplement, Execution: execution}, progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
			if test.wantDeadline {
				if !errors.Is(err, context.DeadlineExceeded) || backend.called {
					t.Fatalf("expired setup invoked provider: called=%t err=%v", backend.called, err)
				}
				return
			}
			if err != nil || decision.Outcome != progresscheck.Reroute || !backend.called || execution.reads != 0 {
				t.Fatalf("decision=%+v err=%v reads=%d", decision, err, execution.reads)
			}
		})
	}
}

type progressSetupExecution struct {
	progressUsageExecution
	advance func()
	reads   int
}

func (e *progressSetupExecution) Start(ctx context.Context, identity tracker.NativeExecutionIdentity) error {
	e.advance()
	return e.progressUsageExecution.Start(ctx, identity)
}

func (e *progressSetupExecution) AgentTools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{{Name: operatortool.WorkItem}}, func(context.Context, AgentToolCall) (AgentToolResult, error) {
		e.reads++
		return AgentToolResult{Success: true, Content: "unexpected late read"}, nil
	}
}

type progressDeadlineBackend struct {
	fakeCodexClient
	t         *testing.T
	now       *time.Time
	remaining time.Duration
	called    bool
}

func (*progressDeadlineBackend) SupportsReadOnlyInvestigation() bool { return true }

func (b *progressDeadlineBackend) RunTurnWithTools(ctx context.Context, req AgentTurnRequest, tools []AgentTool, handler AgentToolHandler, observe AgentUpdateHandler) (AgentTurnResult, error) {
	b.called = true
	if req.MaxDuration != b.remaining || req.TurnTimeout != b.remaining {
		b.t.Fatalf("provider limits=%s/%s want remaining=%s", req.MaxDuration, req.TurnTimeout, b.remaining)
	}
	if !strings.Contains(req.Prompt, "Remaining time: "+b.remaining.String()) {
		b.t.Fatalf("missing remaining time in prompt: %s", req.Prompt)
	}
	*b.now = b.now.Add(b.remaining * 3 / 4)
	for range 2 {
		result, err := req.ToolBudget.Handler(tools, handler)(ctx, AgentToolCall{Name: operatortool.WorkItem, Arguments: json.RawMessage(`{"project_id":"project","reference":"example"}`)})
		if err != nil || result.Success || !strings.Contains(result.Content, "Return your final decision") || ctx.Err() != nil {
			b.t.Fatalf("answer reserve canceled provider or allowed late read: %+v %v %v", result, err, ctx.Err())
		}
	}
	*b.now = b.now.Add(b.remaining / 8)
	return AgentTurnResult{}, observe(AgentUpdate{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"reroute","step":"implement","reason":"Repair the assertion","diagnosis":"failed: assertion failure","instruction":"Repair the failing assertion","evidence":["failed"]}`})
}

type progressWorkspace struct{ *fakeWorkspaceBackend }

func (*progressPolicyBackend) SupportsReadOnlyInvestigation() bool  { return true }
func (*stalledProgressBackend) SupportsReadOnlyInvestigation() bool { return true }
func (*progressInspectBackend) SupportsReadOnlyInvestigation() bool { return true }

func (w progressWorkspace) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return "head", nil
}
func (w progressWorkspace) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}
func (*progressUsageExecution) SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
}
func (*progressUsageExecution) RecoverChangeSource(context.Context) (workspace.ChangeSource, error) {
	return workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base", Source: &tracker.ChangeSource{}}}}, nil
}

func (w progressWorkspace) Existing(workspace.Issue) (workspace.Info, error) { return w.info, nil }

func TestProgressCheckMissingCheckoutUsesWorkerScratch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, "")
	}
	backend := &progressScratchBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"progressing","reason":"Assertion repaired","diagnosis":"failed: TestExample returned the old value","convergence":"TestExample now passes on the repaired source","evidence":["failed"]}`}}}}
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: config.Config{Workspace: config.Workspace{Root: root}}}, Workspace: missingProgressWorkspace{progressWorkspace{&fakeWorkspaceBackend{info: workspace.Info{Path: root}}}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Mode: RunModeImplement, Execution: &progressUsageExecution{}}, progresscheck.Request{ProjectID: "project", IssueID: "example", PolicyID: "policy", EvidenceRevision: "1", SourceSHA: "head", BaseSHA: "base", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backend.request.Prompt, progresscheck.DefaultGuidance) {
		t.Fatal("missing section did not use generic default")
	}
	if !backend.request.ReadOnly || backend.request.TextOnly || backend.request.Workspace != root {
		t.Fatalf("unconfined progress request: %+v", backend.request)
	}
	if _, err := os.Stat(backend.request.TempDir); !os.IsNotExist(err) {
		t.Fatalf("scratch retained: %v", err)
	}
}

type progressScratchBackend struct{ fakeCodexClient }

func (*progressScratchBackend) SupportsReadOnlyInvestigation() bool { return true }

type missingProgressWorkspace struct{ progressWorkspace }

func (w missingProgressWorkspace) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{}, workspace.ErrMissingWorkspace
}

func TestProgressCheckRestoresPinnedSource(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	for _, test := range []struct {
		name, mismatch string
		concurrent     bool
	}{
		{name: "fresh runner restores Hub bundle"},
		{name: "concurrent preparation", concurrent: true},
		{name: "missing retained source uses evidence", mismatch: "missing"},
		{name: "source metadata missing uses evidence", mismatch: "metadata"},
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
			if test.mismatch == "missing" {
				execution.source = workspace.ChangeSource{}
			}
			if test.mismatch == "metadata" {
				execution.source.Version.Source = nil
			}
			local, err := workspace.NewLocalGit(workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: fresh, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			backend := &progressInspectBackend{head: head, evidenceOnly: test.mismatch == "missing" || test.mismatch == "metadata"}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workspace: local, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			if test.concurrent {
				start := make(chan struct{})
				results := make(chan error, 2)
				for range 2 {
					go func() {
						<-start
						_, release, err := r.progressWorkspace(t.Context(), RunRequest{Issue: connector.Issue{ID: "example", Identifier: "example#1"}, Execution: execution}, evidence)
						if err == nil {
							release()
						}
						results <- err
					}()
				}
				close(start)
				for range 2 {
					if err := <-results; err != nil {
						t.Error(err)
					}
				}
				return
			}
			_, err = r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example", Identifier: "example#1"}, Mode: RunModeImplement, Execution: execution}, evidence)
			if test.mismatch == "head" || test.mismatch == "base" {
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
	head         string
	called       bool
	evidenceOnly bool
}

func (b *progressInspectBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.called = true
	if b.evidenceOnly {
		if req.Workspace != "" || !req.ReadOnly || !req.IsolatedTools || !strings.Contains(req.Prompt, "evidence-only investigation") {
			return AgentTurnResult{}, errors.New("evidence-only investigation escaped isolation")
		}
		return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"reroute","step":"implement","reason":"Obtain missing source","diagnosis":"failed: retained source is unavailable","instruction":"Capture the failing source and rerun its validation to diagnose the failure","evidence":["failed"]}`})
	}
	content, err := os.ReadFile(filepath.Join(req.Workspace, "assertion.txt"))
	if err != nil || string(content) != "failing assertion and exercised code\n" || !req.ReadOnly || req.TextOnly || !req.IsolatedTools || !strings.Contains(req.Prompt, b.head) {
		return AgentTurnResult{}, errors.New("pinned source unavailable to read-only investigation")
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"reroute","step":"implement","reason":"Repair the failing assertion","diagnosis":"failed: assertion.txt retains the failing assertion","instruction":"Change the implementation exercised by assertion.txt and run the matching validation command","evidence":["failed"]}`})
}

func TestProgressToolBudgetBoundsAllReads(t *testing.T) {
	for _, test := range []struct {
		name         string
		calls, bytes int
		result       string
		native       bool
	}{
		{"call count", 1, 1000, "small", false},
		{"cumulative bytes", 10, 7, "four", false},
		{"host and provider call count", 1, 1000, "small", true},
		{"host and provider bytes", 10, 7, "four", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			budget := NewAgentToolBudget(test.calls, test.bytes)
			reads := 0
			handle := budget.handler(func(context.Context, AgentToolCall) (AgentToolResult, error) {
				reads++
				return AgentToolResult{Success: true, Content: test.result}, nil
			})
			if _, err := handle(ctx, AgentToolCall{}); err != nil {
				t.Fatal(err)
			}
			var result AgentToolResult
			var err error
			if test.native {
				typeOfUpdate := AgentUpdateToolCompleted
				if test.calls == 1 {
					typeOfUpdate = AgentUpdateToolStarted
				}
				err = budget.Observe(AgentUpdate{Type: typeOfUpdate, Delta: test.result})
			} else {
				result, err = handle(ctx, AgentToolCall{})
			}
			if !errors.Is(err, ErrInvestigationBudget) || result.Content != "" || context.Cause(ctx) != nil {
				t.Fatalf("exhaustion leaked result or continued: result=%+v err=%v cause=%v", result, err, context.Cause(ctx))
			}
			if _, err := handle(ctx, AgentToolCall{}); !errors.Is(err, ErrInvestigationBudget) {
				t.Fatal(err)
			}
			want := 2
			if test.calls == 1 || test.native {
				want = 1
			}
			if reads != want {
				t.Fatalf("reads=%d, want %d", reads, want)
			}
		})
	}
}

func TestProgressCheckRefusesUnsupportedBackend(t *testing.T) {
	backend := &fakeCodexClient{}
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workspace: progressWorkspace{&fakeWorkspaceBackend{}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	execution := &progressUsageExecution{}
	_, err = r.CheckProgress(t.Context(), RunRequest{Execution: execution}, progresscheck.Request{})
	if err == nil || !strings.Contains(err.Error(), "cannot enforce") || execution.started || backend.request.Prompt != "" {
		t.Fatalf("unsupported investigation invoked provider: err=%v started=%t", err, execution.started)
	}
}

func TestProgressPreparationNeverFallsBackWithoutRetainedSource(t *testing.T) {
	for _, test := range []struct {
		name      string
		execution Execution
		sourceSHA string
	}{
		{"execution lacks source capability", &recordingUsageExecution{}, "head"},
		{"empty recovered version", &progressSourceExecution{}, "head"},
		{"metadata missing", &progressSourceExecution{source: workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base"}}}}, "head"},
		{"source SHA missing", &progressUsageExecution{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &progressFallbackWorkspace{progressWorkspace: progressWorkspace{&fakeWorkspaceBackend{}}}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workspace: backend, AgentBackend: &progressScratchBackend{}})
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := r.progressWorkspace(t.Context(), RunRequest{Issue: connector.Issue{ID: "example"}, Execution: test.execution}, progresscheck.Request{SourceSHA: test.sourceSHA, BaseSHA: "base"})
			if release != nil {
				release()
			}
			if err != nil || backend.created || backend.seeded {
				t.Fatalf("unavailable retained source reached fallback: err=%v created=%t seeded=%t", err, backend.created, backend.seeded)
			}
		})
	}
}

type progressFallbackWorkspace struct {
	progressWorkspace
	seeded bool
}

func (w *progressFallbackWorkspace) SeedReviewHead(context.Context, workspace.Info, workspace.Issue) error {
	w.seeded = true
	return nil
}

func TestProgressToolBudgetCountsCompletedResultsOnce(t *testing.T) {
	budget := NewAgentToolBudget(3, 11)
	handler := budget.Handler([]AgentTool{{Name: "work_item"}}, func(context.Context, AgentToolCall) (AgentToolResult, error) {
		return AgentToolResult{Success: true, Content: "four"}, nil
	})
	if result, err := handler(t.Context(), AgentToolCall{Name: "work_item"}); err != nil || !result.Success {
		t.Fatalf("read failed: %+v %v", result, err)
	}
	for _, update := range []AgentUpdate{
		{Type: AgentUpdateToolStarted, ItemID: "host-1", Tool: "work_item"},
		{Type: AgentUpdateToolCompleted, ItemID: "host-1", Tool: "work_item", Delta: "four"},
		{Type: AgentUpdateToolStarted, ItemID: "read-1"},
		{Type: AgentUpdateToolStarted, ItemID: "read-1"},
		{Type: AgentUpdateToolOutput, ItemID: "read-1", Delta: "four"},
		{Type: AgentUpdateToolCompleted, ItemID: "read-1", Delta: "four"},
		{Type: AgentUpdateToolCompleted, ItemID: "read-1", Delta: "four"},
		{Type: AgentUpdateToolStarted, ItemID: "read-2"},
		{Type: AgentUpdateToolCompleted, ItemID: "read-2", Delta: "two"},
	} {
		if err := budget.Observe(update); err != nil {
			t.Fatalf("double-counted tool lifecycle: %v", err)
		}
	}
	if budget.calls != 3 || budget.bytes != 11 {
		t.Fatalf("calls=%d bytes=%d, want 3 and 11", budget.calls, budget.bytes)
	}
}

func TestProgressReadDeadlinePreservesAnswerContext(t *testing.T) {
	for _, parentCanceled := range []bool{false, true} {
		t.Run(strconv.FormatBool(parentCanceled), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				budget := NewAgentToolBudget(80, 1000)
				budget.readDeadline = time.Now().Add(15 * time.Second)
				reads := 0
				handle := budget.Handler([]AgentTool{{Name: "work_item"}}, func(readCtx context.Context, _ AgentToolCall) (AgentToolResult, error) {
					reads++
					if parentCanceled {
						cancel()
					}
					<-readCtx.Done()
					return AgentToolResult{}, readCtx.Err()
				})
				for range 2 {
					result, err := handle(ctx, AgentToolCall{Name: "work_item"})
					if parentCanceled {
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("lost cancellation: %+v %v", result, err)
						}
					} else if err != nil || result.Success || !strings.Contains(result.Content, "Return your final decision") || ctx.Err() != nil || time.Until(mustProgressDeadline(ctx)) != 5*time.Second {
						t.Fatalf("read consumed answer reserve: %+v err=%v cause=%v", result, err, context.Cause(ctx))
					}
				}
				if reads != 1 {
					t.Fatalf("late read executed: %d", reads)
				}
			})
		})
	}
}

func mustProgressDeadline(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func (b *progressInspectBackend) RunTurnWithTools(ctx context.Context, req AgentTurnRequest, tools []AgentTool, handle AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	if !b.evidenceOnly {
		for _, tool := range tools {
			if tool.Name != "detent_read_source" {
				return AgentTurnResult{}, errors.New("unexpected investigation tool")
			}
		}
		result, err := handle(ctx, AgentToolCall{Name: "detent_read_source", Arguments: json.RawMessage(`{"operation":"read","revision":"head","path":"assertion.txt"}`)})
		if err != nil || !result.Success || !strings.Contains(result.Content, "failing assertion and exercised code") {
			return AgentTurnResult{}, fmt.Errorf("pinned source tool unavailable: %w, %s", err, result.Content)
		}
		result, err = handle(ctx, AgentToolCall{Name: "detent_run_validation", Arguments: json.RawMessage(`{"command":"printf changed > assertion.txt"}`)})
		if err == nil || result.Success {
			return AgentTurnResult{}, errors.New("investigation permitted validation command")
		}
	}
	return b.RunTurn(ctx, req, update)
}

func TestProgressCheckWithoutSourceInvestigatesScopedDiagnostics(t *testing.T) {
	root := t.TempDir()
	backend := &progressPolicyBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: `{"outcome":"reroute","step":"implement","reason":"Repair failure","diagnosis":"failed-attempt: TestExample returned the old value","instruction":"Repair the implementation exercised by TestExample and run its validation","evidence":["failed-attempt"]}`}}}}
	workspaces := &progressFallbackWorkspace{progressWorkspace: progressWorkspace{&fakeWorkspaceBackend{info: workspace.Info{Path: root}}}}
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workspace: workspaces, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	execution := &progressEvidenceExecution{}
	decision, err := r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example-issue", IsolationPolicy: &isolation.Policy{Tier: isolation.NativeTrusted}}, Mode: RunModeImplement, Execution: execution}, progresscheck.Request{ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision", SourceSHA: "head", BaseSHA: "base", Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
	if err != nil || decision.Outcome != progresscheck.Reroute {
		t.Fatalf("evidence-only decision=%+v err=%v", decision, err)
	}
	if backend.request.Workspace != "" || !backend.request.IsolatedTools || !backend.request.ReadOnly || workspaces.created || workspaces.seeded {
		t.Fatalf("evidence-only investigation prepared source: %+v", backend.request)
	}
	if !strings.Contains(backend.request.Prompt, "TestExample: expected repaired value, got old value") || !strings.Contains(backend.request.Prompt, "evidence-only investigation") || execution.reads != 2 || execution.writes != 0 || !backend.refusedWrite {
		t.Fatalf("scoped diagnostics missing: reads=%d writes=%d prompt=%s", execution.reads, execution.writes, backend.request.Prompt)
	}
	if _, err := os.Stat(backend.request.TempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch retained: %v", err)
	}
}

type progressEvidenceExecution struct{ progressReadExecution }

func (*progressEvidenceExecution) RecoverChangeSource(context.Context) (workspace.ChangeSource, error) {
	return workspace.ChangeSource{}, nil
}
