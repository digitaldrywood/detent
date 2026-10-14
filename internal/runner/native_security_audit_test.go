package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeSecurityAuditTestExecution struct {
	attachmentValidatorExecution
	audit    *tracker.SecurityAuditReview
	versions int
}

func (e *nativeSecurityAuditTestExecution) SecurityAuditVersion(ctx context.Context) (NativeValidation, error) {
	e.versions++
	return e.ValidatorVersion(ctx)
}

func (e *nativeSecurityAuditTestExecution) RecordSecurityAudit(_ context.Context, review tracker.SecurityAuditReview) error {
	e.audit = &review
	return nil
}

func TestNativeSecurityAuditStage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                       string
		kind, authentication       string
		disabled, blocking, failed bool
		landing                    bool
		processClockOffset         time.Duration
	}{
		{name: "default native Codex subscription pass", kind: config.AgentBackendCodex, authentication: "chatgpt_subscription"},
		{name: "default native Codex metered pass", kind: config.AgentBackendCodex, authentication: "api_key"},
		{name: "native OS identity clock precedes audit clock", processClockOffset: -2 * time.Second},
		{name: "native OS identity clock follows audit clock", processClockOffset: 2 * time.Second},
		{name: "default native Claude metered pass", kind: config.AgentBackendClaudeCode, authentication: "api_key"},
		{name: "default native Claude subscription pass", kind: config.AgentBackendClaudeCode, authentication: "subscription"},
		{name: "default landing Claude metered pass", kind: config.AgentBackendClaudeCode, authentication: "api_key", landing: true},
		{name: "blocking finding", blocking: true},
		{name: "explicit false", disabled: true},
		{name: "backend failure", failed: true},
		{name: "landing exact integrated identity", landing: true},
		{name: "landing blocking finding", landing: true, blocking: true},
		{name: "landing backend failure", landing: true, failed: true},
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
				output = `{"verdict":"fail","summary":"Authorization bypass","findings":[{"id":"auth","severity":"p1","body":"Missing authorization"}]}`
			}
			backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: now.Add(time.Second + test.processClockOffset)}}, {Type: AgentUpdateMessageDelta, Delta: output}}}
			backend.result.AuthenticationMode = authentication
			if test.failed {
				backend.err = errors.New("backend unavailable")
			}
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			if test.disabled {
				cfg.Gate.SecurityAudit.Enabled = false
			}
			cfg.Gate.AutomatedReview = gate.AutomatedReviewOff
			cfg.Agents.ModelSelection.Enabled = new(false)
			cfg.Agents.Backends = []config.AgentBackend{{ID: "audit", Kind: kind, Protocol: "app-server", Command: "test-audit"}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "audit", Role: RoleSecurityAudit, Backend: "audit", Model: "audit-model", Default: true}}
			runner, err := NewRunner(Dependencies{ProjectID: "prj_example", Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{}, AgentBackends: map[string]AgentBackend{"audit": backend}, SecurityAuditRoot: t.TempDir(), Store: &fakeSessionStore{sessionID: 73}, Now: func() time.Time { return now.Add(time.Second) }})
			if err != nil {
				t.Fatal(err)
			}
			execution := &nativeSecurityAuditTestExecution{}
			req := RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1"}}
			var approved bool
			if test.landing {
				var review *tracker.SecurityAuditReview
				review, err = runner.auditNativeLanding(t.Context(), req, NativeLandingTarget{VersionID: "version", Repository: "https://github.com/example-org/example-project"}, workspace.FileDiffs{BaseSHA: "base", HeadSHA: "head", Files: []workspace.FileDiff{{Path: "file.go", Patch: "+change"}}})
				approved = review != nil && review.Verdict == "pass"
				if err == nil && (review == nil || !review.Landing || review.VersionID != "version" || execution.versions != 0) {
					t.Fatalf("landing audit lost integration identity: %+v", review)
				}
			} else {
				approved, err = runner.auditNativeChange(t.Context(), req)
			}
			if (err != nil) != test.failed {
				t.Fatalf("audit error = %v", err)
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
			if backend.calls != 1 || execution.audit == nil || execution.audit.Run.BaseSHA != "base" || execution.audit.Run.HeadSHA != "head" || execution.audit.Run.ReviewerDigest == "" || execution.audit.SessionID != 73 || execution.audit.Identity.Role != RoleSecurityAudit || execution.audit.Identity.BackendKind != kind || execution.audit.Identity.BackendID != "audit" || execution.audit.Run.AuthenticationMode != authentication || backend.request.Model != "audit-model" {
				t.Fatalf("audit evidence = %+v; turns=%d", execution.audit, backend.calls)
			}
		})
	}
}
