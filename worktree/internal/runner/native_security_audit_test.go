package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeSecurityAuditTestExecution struct {
	attachmentValidatorExecution
	audit          *tracker.SecurityAuditReview
	versions       int
	workspace      func() string
	submitErr      error
	publicationErr error
	evidence       []ValidationEvidence
	diff           string
}

type nativeSecurityAuditIsolationBackend struct {
	*fakeCodexClient
	policy isolation.Policy
}

func (b *nativeSecurityAuditIsolationBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	policy, ok := isolation.FromContext(ctx)
	if !ok {
		return AgentTurnResult{}, errors.New("runner claim isolation policy is unavailable")
	}
	if err := policy.Validate(); err != nil {
		return AgentTurnResult{}, err
	}
	b.policy = policy
	return b.fakeCodexClient.RunTurn(ctx, req, update)
}

func (e *nativeSecurityAuditTestExecution) SecurityAuditVersion(ctx context.Context) (NativeValidation, error) {
	e.versions++
	input, err := e.ValidatorVersion(ctx)
	if e.diff != "" && input.Diff != nil {
		input.Diff.Patch = e.diff
	}
	return input, err
}

func (e *nativeSecurityAuditTestExecution) RecordSecurityAudit(_ context.Context, review tracker.SecurityAuditReview) error {
	if _, err := os.Stat(e.workspace()); err != nil {
		return errors.New("audit workspace disappeared before submission")
	}
	if e.submitErr != nil {
		return e.submitErr
	}
	// Match the Hub admission boundary: local runner configuration is not
	// authority for the native project or work item named by audit evidence.
	if review.Run.ProjectID != string(e.recovery.Issue.ProjectID) || review.Run.IssueID != string(e.recovery.Issue.WorkItemID) {
		return errors.New("security audit identity differs from the work item")
	}
	e.audit = &review
	return nil
}

func (e *nativeSecurityAuditTestExecution) PublishValidationEvidence(_ context.Context, files []ValidationEvidence) error {
	if _, err := os.Stat(e.workspace()); err != nil {
		return errors.New("audit workspace disappeared before publication")
	}
	e.evidence = files
	return e.publicationErr
}

func TestNativeSecurityAuditStage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                        string
		kind, authentication        string
		disabled, blocking, failed  bool
		landing                     bool
		claimPolicy, ambientPolicy  bool
		nonNative                   bool
		processClockOffset          time.Duration
		rejected, publicationFailed bool
		missingProcess              bool
		severity                    string
		localProjectID              string
		large                       bool
	}{
		{name: "source audit uses claimed native project", localProjectID: "example-local-key"},
		{name: "landing audit uses claimed native project", landing: true, localProjectID: "example-local-key"},
		{name: "default native Codex subscription pass", kind: config.AgentBackendCodex, authentication: "chatgpt_subscription"},
		{name: "default native Codex metered pass", kind: config.AgentBackendCodex, authentication: "api_key"},
		{name: "native OS identity clock precedes audit clock", processClockOffset: -2 * time.Second},
		{name: "native OS identity clock follows audit clock", processClockOffset: 2 * time.Second},
		{name: "default native Claude metered pass", kind: config.AgentBackendClaudeCode, authentication: "api_key"},
		{name: "default native Claude subscription pass", kind: config.AgentBackendClaudeCode, authentication: "subscription"},
		{name: "default landing Claude metered pass", kind: config.AgentBackendClaudeCode, authentication: "api_key", landing: true},
		{name: "blocking finding", blocking: true},
		{name: "blocking p2 finding", blocking: true, severity: "p2"},
		{name: "incomplete process identity publishes diagnostics", missingProcess: true, failed: true},
		{name: "Hub rejection publishes diagnostics", rejected: true, failed: true},
		{name: "landing Hub rejection publishes diagnostics", rejected: true, failed: true, landing: true},
		{name: "publication failure retains diagnostics", rejected: true, publicationFailed: true, failed: true},
		{name: "landing publication failure retains diagnostics", rejected: true, publicationFailed: true, failed: true, landing: true},
		{name: "native complete large diff", large: true},
		{name: "landing complete large diff", large: true, landing: true},
		{name: "explicit false", disabled: true},
		{name: "backend failure", failed: true},
		{name: "landing exact integrated identity", landing: true},
		{name: "landing blocking finding", landing: true, blocking: true},
		{name: "landing backend failure", landing: true, failed: true},
		{name: "landing retains member claim isolation", landing: true, claimPolicy: true},
		{name: "landing replaces another member isolation", landing: true, claimPolicy: true, ambientPolicy: true},
		{name: "native audit refuses missing member isolation", landing: true, ambientPolicy: true, failed: true},
		{name: "standalone audit retains ambient isolation", landing: true, ambientPolicy: true, nonNative: true},
		{name: "standalone audit prefers explicit claim isolation", landing: true, claimPolicy: true, ambientPolicy: true, nonNative: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			kind, authentication := test.kind, test.authentication
			if kind == "" {
				kind = config.AgentBackendCodex
			}
			if authentication == "" {
				authentication = "api_key"
			}
			output := `{"verdict":"pass","summary":"No findings","findings":[]}`
			if test.blocking {
				output = `{"verdict":"fail","summary":"Authorization bypass","findings":[{"id":"auth","certainty":"confirmed","severity":"p1","body":"Missing authorization"}]}`
			}
			if test.severity != "" {
				output = strings.ReplaceAll(output, `"p1"`, `"`+test.severity+`"`)
			}
			backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: now.Add(time.Second + test.processClockOffset)}}, {Type: AgentUpdateMessageDelta, Delta: output}}}
			if test.missingProcess {
				backend.updates[0].WorkerProcess.PID = 0
			}
			backend.result.AuthenticationMode = authentication
			if test.failed && !test.ambientPolicy && !test.rejected && !test.missingProcess {
				backend.err = errors.New("backend unavailable")
			}
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			if test.nonNative {
				cfg.Tracker.Kind = config.TrackerGitHub
			}
			if test.disabled {
				cfg.Gate.SecurityAudit.Enabled = false
			}
			cfg.Gate.AutomatedReview = gate.AutomatedReviewOff
			cfg.Agents.ModelSelection.Enabled = new(false)
			cfg.Agents.Backends = []config.AgentBackend{{ID: "audit", Kind: kind, Protocol: "app-server", Command: "test-audit"}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "audit", Role: RoleSecurityAudit, Backend: "audit", Model: "audit-model", Default: true}}
			var configuredBackend AgentBackend = backend
			var isolatedBackend *nativeSecurityAuditIsolationBackend
			var chunks *chunkAuditBackend
			if test.large {
				chunks = &chunkAuditBackend{t: t, mode: "pass"}
				configuredBackend = chunks
			}
			if test.claimPolicy || test.ambientPolicy {
				isolatedBackend = &nativeSecurityAuditIsolationBackend{fakeCodexClient: backend}
				configuredBackend = isolatedBackend
			}
			localProjectID := test.localProjectID
			if localProjectID == "" {
				localProjectID = "prj_example"
			}
			runner, err := NewRunner(Dependencies{ProjectID: localProjectID, Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{}, AgentBackends: map[string]AgentBackend{"audit": configuredBackend}, SecurityAuditRoot: t.TempDir(), Store: &fakeSessionStore{sessionID: 73}, Now: func() time.Time { return now.Add(time.Second) }})
			if err != nil {
				t.Fatal(err)
			}
			execution := &nativeSecurityAuditTestExecution{workspace: func() string {
				if chunks != nil && len(chunks.requests) > 0 {
					return chunks.requests[len(chunks.requests)-1].Workspace
				}
				return backend.request.Workspace
			}}
			execution.recovery.Issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example"}}
			if test.rejected {
				execution.submitErr = errors.New("Hub rejected completed audit")
			}
			if test.publicationFailed {
				execution.publicationErr = errors.New("Hub artifact unavailable")
			}
			patch := "+change"
			if test.large {
				patch = "diff --git a/file.go b/file.go\n@@ -0,0 +1 @@\n" + strings.Repeat("+record\n", 80000)
				execution.diff = patch
			}
			req := RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1"}}
			ctx := t.Context()
			ambient := isolation.Policy{Tier: isolation.NativeTrusted, HostServices: []string{"unix:/tmp/another-member.sock"}, ExtraNetworkDomains: []string{"another-member.example"}}
			if test.ambientPolicy {
				ctx = isolation.WithPolicy(ctx, ambient)
			}
			if test.claimPolicy {
				req.Issue.IsolationPolicy = &isolation.Policy{Tier: isolation.Sandbox, WritableRoots: []string{t.TempDir()}, HostServices: []string{"unix:/tmp/member.sock"}, ExtraNetworkDomains: []string{"member.example"}, AllowLocalBinding: true}
			}
			var approved bool
			if test.landing {
				var review *tracker.SecurityAuditReview
				review, err = runner.auditNativeLanding(ctx, req, NativeLandingTarget{VersionID: "version", Repository: "https://github.com/example-org/example-project"}, workspace.FileDiffs{BaseSHA: "base", HeadSHA: "head", Files: []workspace.FileDiff{{Path: "file.go", Patch: patch}}})
				approved = review != nil && review.Verdict == "pass"
				if err == nil && (review == nil || !review.Landing || review.VersionID != "version" || execution.versions != 0) {
					t.Fatalf("landing audit lost integration identity: %+v", review)
				}
			} else {
				approved, err = runner.auditNativeChange(ctx, req)
			}
			if (err != nil) != test.failed {
				t.Fatalf("audit error = %v", err)
			}
			if test.rejected || test.missingProcess {
				if len(execution.evidence) != 1 {
					t.Fatalf("diagnostic evidence = %+v; error=%v", execution.evidence, err)
				}
				var diagnostic struct {
					Run           struct{ ID int64 }
					Output, Error string
				}
				wantFailure := "Hub rejected completed audit"
				if test.missingProcess {
					wantFailure = "worker_pid"
				}
				if decodeErr := json.Unmarshal(execution.evidence[0].Content, &diagnostic); decodeErr != nil || diagnostic.Run.ID != 73 || diagnostic.Output != output || !strings.Contains(diagnostic.Error, wantFailure) {
					t.Fatalf("diagnostic = %+v, %v", diagnostic, decodeErr)
				}
				_, statErr := os.Stat(filepath.Join(backend.request.Workspace, execution.evidence[0].Name))
				if test.publicationFailed {
					if statErr != nil || !strings.Contains(err.Error(), backend.request.Workspace) {
						t.Fatalf("lost unpublished diagnostics: %v, %v", statErr, err)
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("published workspace cleanup = %v", statErr)
				}
			}
			if test.failed {
				if !errors.Is(err, ErrValidatorInfrastructure) || execution.audit != nil {
					t.Fatalf("backend failure became a finding: %+v, %v", execution.audit, err)
				}
				return
			}
			if approved == test.blocking {
				t.Fatalf("approved = %t", approved)
			}
			if test.disabled {
				if backend.calls != 0 || execution.versions != 0 || execution.audit != nil {
					t.Fatal("disabled audit ran")
				}
				return
			}
			if isolatedBackend != nil {
				wantPolicy := ambient
				if req.Issue.IsolationPolicy != nil {
					wantPolicy = *req.Issue.IsolationPolicy
				}
				if !reflect.DeepEqual(isolatedBackend.policy, wantPolicy) {
					t.Fatalf("audit isolation = %+v, want %+v", isolatedBackend.policy, wantPolicy)
				}
			}
			if test.large {
				if len(chunks.requests) < 2 || chunks.covered.String() != patch || execution.audit == nil || execution.audit.Run.HeadSHA != "head" || execution.audit.Run.BaseSHA != "base" || execution.audit.SessionID != 73 || execution.audit.DiffDigest == "" {
					t.Fatal("native aggregate lost complete source or immutable receipt identity")
				}
				return
			}
			if backend.calls != 1 || execution.audit == nil || execution.audit.Run.BaseSHA != "base" || execution.audit.Run.HeadSHA != "head" || execution.audit.Run.ReviewerDigest == "" || execution.audit.SessionID != 73 || execution.audit.Identity.Role != RoleSecurityAudit || execution.audit.Identity.BackendKind != kind || execution.audit.Identity.BackendID != "audit" || execution.audit.Run.AuthenticationMode != authentication || backend.request.Model != "audit-model" {
				t.Fatalf("audit evidence = %+v; turns=%d", execution.audit, backend.calls)
			}
			if execution.audit.Run.ProjectID != string(execution.recovery.Issue.ProjectID) {
				t.Fatalf("audit project = %q, want claimed project %q", execution.audit.Run.ProjectID, execution.recovery.Issue.ProjectID)
			}
		})
	}
}

type verificationAuditSessionStore struct{ *fakeSessionStore }

func (s *verificationAuditSessionStore) StartSession(ctx context.Context, attrs store.SessionStart) (int64, error) {
	id, err := s.fakeSessionStore.StartSession(ctx, attrs)
	return id + int64(s.startCalls-1), err
}

type verificationAuditBackend struct {
	fakeCodexClient
	t                *testing.T
	outputs          []string
	requests         []AgentTurnRequest
	deadline         time.Time
	failVerification bool
}

func (b *verificationAuditBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || !b.deadline.IsZero() && !deadline.Equal(b.deadline) {
		b.t.Fatal("verification reset the stage deadline")
	}
	b.deadline = deadline
	if !agentResumeEmpty(req.Resume) || !req.ReadOnly || !strings.Contains(req.ToolInstructions, "Use no tools") {
		b.t.Fatal("verification weakened the fresh no-tools audit boundary")
	}
	b.requests = append(b.requests, req)
	if len(b.requests) == 2 {
		if !strings.Contains(req.Prompt, `"verification_of"`) || !strings.Contains(req.Prompt, `"textual_diff":"+change"`) || !strings.Contains(req.Prompt, `"id":"auth"`) {
			b.t.Fatal("verification lost supplied evidence or lead identity")
		}
		if b.failVerification {
			return AgentTurnResult{}, errors.New("verification provider unavailable")
		}
	}
	b.updates = []AgentUpdate{
		{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: time.Now().UTC()}},
		{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{TotalTokens: 10, ThreadTotal: &AgentTokenCounts{TotalTokens: 10}}},
		{Type: AgentUpdateMessageDelta, Delta: b.outputs[len(b.requests)-1]},
	}
	return b.fakeCodexClient.RunTurn(ctx, req, update)
}

func TestNativeSecurityAuditVerification(t *testing.T) {
	t.Parallel()
	confirmed := `{"verdict":"fail","summary":"Confirmed authorization bypass","findings":[{"id":"auth","certainty":"confirmed","severity":"p1","body":"Attacker reaches unchecked handler","status":"unresolved"}]}`
	undetermined := `{"verdict":"undetermined","summary":"Potential cross-tenant access","findings":[{"id":"auth","certainty":"undetermined","body":"A p2 lead requires caller context","unresolved_fact":"Whether the caller enforces tenant authorization","path":"auth.go","status":"unresolved"}]}`
	disproved := `{"verdict":"pass","summary":"Lead disproved","findings":[{"id":"auth","certainty":"disproved","body":"The supplied caller validates tenant authorization","status":"rejected"}]}`
	for _, tt := range []struct {
		name, initial, final, verdict string
		failed                        bool
		unavailable, alreadyComplex   bool
		catalogFailure                bool
	}{
		{name: "confirmed first session", initial: confirmed, verdict: "rework"},
		{name: "higher model confirms p2", initial: undetermined, final: strings.ReplaceAll(confirmed, "p1", "p2"), verdict: "rework"},
		{name: "higher model rejects", initial: undetermined, final: disproved, verdict: "pass"},
		{name: "higher model cannot decide", initial: undetermined, final: undetermined, verdict: "wait"},
		{name: "verifier failure stays instance owned", initial: undetermined, final: undetermined, failed: true},
		{name: "verifier omission stays instance owned", initial: undetermined, final: `{"verdict":"pass","summary":"Safe","findings":[]}`, failed: true},
		{name: "complex model unavailable", initial: undetermined, failed: true, unavailable: true},
		{name: "complex catalog unavailable", initial: undetermined, failed: true, catalogFailure: true},
		{name: "already complex still gets fresh verification", initial: undetermined, final: undetermined, verdict: "wait", alreadyComplex: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &verificationAuditBackend{t: t, outputs: []string{tt.initial}, failVerification: tt.failed && tt.name == "verifier failure stays instance owned"}
			if tt.final != "" {
				backend.outputs = append(backend.outputs, tt.final)
			}
			backend.models = []AgentModel{{ID: "complex-model", Model: "complex-model", SupportedReasoningEfforts: []string{"low", "medium", "high"}}}
			if tt.unavailable {
				backend.models = nil
			}
			if tt.catalogFailure {
				backend.catalogErr = errors.New("catalog unavailable")
			}
			cfg := config.Default()
			cfg.Agents.ModelSelection.Enabled = new(false)
			cfg.Agents.ModelSelection.ComplexModel = new("complex-model")
			cfg.Agents.Backends = []config.AgentBackend{{ID: "audit", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-audit"}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "audit", Role: RoleSecurityAudit, Backend: "audit", Model: "audit-model", Default: true}}
			initialModel := "audit-model"
			if tt.alreadyComplex {
				initialModel = "complex-model"
				cfg.Agents.Routes[0].Model = initialModel
			}
			sessions := &verificationAuditSessionStore{fakeSessionStore: &fakeSessionStore{sessionID: 73}}
			runner, err := NewRunner(Dependencies{ProjectID: "prj_example", Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{}, AgentBackends: map[string]AgentBackend{"audit": backend}, SecurityAuditRoot: t.TempDir(), Store: sessions})
			if err != nil {
				t.Fatal(err)
			}
			execution := &nativeSecurityAuditTestExecution{diff: "+change", workspace: func() string { return backend.requests[len(backend.requests)-1].Workspace }}
			execution.recovery.Issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example"}}
			var usage []int64
			approved, err := runner.auditNativeChange(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example"}, OnUsageUpdate: func(update UsageUpdate) error { usage = append(usage, update.Tokens.TotalTokens); return nil }})
			if tt.failed {
				if !errors.Is(err, ErrValidatorInfrastructure) || execution.audit != nil || approved {
					t.Fatalf("verification failure became a result: %+v, %v", execution.audit, err)
				}
				if (tt.unavailable || tt.catalogFailure) && (len(backend.requests) != 1 || sessions.startCalls != 1) {
					t.Fatal("unavailable complex model started a fallback session")
				}
				return
			}
			if err != nil || execution.audit == nil || execution.audit.Verdict != tt.verdict || approved != (tt.verdict == "pass") {
				t.Fatalf("audit = %+v, approved=%v, error=%v", execution.audit, approved, err)
			}
			calls := len(backend.outputs)
			if len(backend.requests) != calls || sessions.startCalls != calls || sessions.finishCalls != calls {
				t.Fatalf("audit session counts: requests=%d starts=%d finishes=%d", len(backend.requests), sessions.startCalls, sessions.finishCalls)
			}
			if usage[len(usage)-1] != int64(10*calls) {
				t.Fatalf("stage usage = %v", usage)
			}
			if execution.audit.SessionID != int64(72+calls) {
				t.Fatal("verification reused the original Detent session")
			}
			if calls == 2 {
				if backend.requests[0].Model != initialModel || backend.requests[1].Model != "complex-model" || backend.requests[0].Workspace == backend.requests[1].Workspace {
					t.Fatal("verification did not select the configured higher model in a new session")
				}
				if execution.audit.Run.Verification == nil || execution.audit.Run.Verification.Initial.Findings[0].Certainty != securityaudit.CertaintyUndetermined {
					t.Fatal("initial verdict was not retained on the Change")
				}
			}
			if tt.verdict == "pass" && (execution.audit.Run.Findings[0].Status != "rejected" || execution.audit.Run.Findings[0].Body == "") {
				t.Fatal("rejected lead or reasoning was discarded")
			}
			action := execution.audit.Run.HumanAction()
			if tt.verdict == "wait" {
				for _, part := range []string{"auth", "first verdict undetermined", "higher-model verdict undetermined", "Whether the caller enforces tenant authorization"} {
					if !strings.Contains(action, part) {
						t.Fatalf("human action missing %q: %s", part, action)
					}
				}
			} else if action != "" {
				t.Fatalf("unexpected human action: %s", action)
			}
			if sessions.finished.TotalTokens != 10 {
				t.Fatalf("session accounting double-counted verification: %+v", sessions.finished)
			}
		})
	}
}
