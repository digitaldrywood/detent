package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeSecurityAuditReusesUnchangedVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		decisions []string
		landing   bool
		oldPrompt bool
		blocked   bool
		human     bool
	}{
		{name: "undecidable audit retains human hold", decisions: []string{"commented"}, human: true},
		{name: "approved", decisions: []string{"approved"}},
		{name: "blocking audit", decisions: []string{"changes_requested"}, blocked: true},
		{name: "earlier pass does not clear block", decisions: []string{"approved", "changes_requested"}, blocked: true},
		{name: "later pass does not clear block", decisions: []string{"changes_requested", "approved"}, blocked: true},
		{name: "blocking landing audit", decisions: []string{"changes_requested"}, landing: true, blocked: true},
		{name: "prompt update does not clear finding", decisions: []string{"changes_requested"}, oldPrompt: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example-org/example-project", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), PolicyID: "policy_example"}, Policy: policy.Descriptor{Gates: policy.Gates{SecurityAudit: true}}}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_example", ProjectID: "prj_example", WorkItemID: "wi_example", CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}}
			for _, decision := range tc.decisions {
				audit := &tracker.SecurityAuditReview{Landing: tc.landing, Verdict: "pass", Run: securityaudit.Run{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest()}}
				if decision == "changes_requested" {
					audit.Verdict = "rework"
					audit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "Authorization bypass"}}
				}
				if tc.human {
					audit.Verdict = "wait"
					audit.Run.Verdict = securityaudit.VerdictUndetermined
					audit.Run.Findings = []securityaudit.Finding{{ID: "auth", Certainty: securityaudit.CertaintyUndetermined, Body: "Potential p2 cross-tenant access", UnresolvedFact: "Whether the caller validates tenant authorization"}}
				}
				if tc.oldPrompt {
					audit.Run.ReviewerDigest = "old"
				}
				detail.Reviews = append(detail.Reviews, tracker.ChangeReview{VersionID: version.ID, Decision: decision, SecurityAudit: audit})
			}
			client, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/changes/change_example") {
					t.Errorf("unchanged terminal audit requested fresh execution evidence: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				page := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Detail: &detail, Read: &operatortool.ChangeRead{VersionID: version.ID, Sections: map[string]operatortool.ChangeReadPage{}}}
				for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
					page.Read.Sections[section] = operatortool.ChangeReadPage{Complete: true}
				}
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			execution := &nativeExecution{claim: nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: "wi_example"}}, change: &runner.NativeChange{ChangeID: detail.Change.ID, VersionID: version.ID, Reviewed: true}, data: tracker.NativeRunData{PolicyID: version.PolicyID}, lastDiff: &tracker.AttemptDiffRequest{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}}
			input, err := execution.SecurityAuditVersion(t.Context())
			if err != nil || input.Version != nil {
				t.Fatalf("unchanged terminal audit was scheduled again: %+v, %v", input, err)
			}
			if tc.human {
				disposition := execution.preparedDisposition
				if disposition == nil || disposition.Status != "blocked" || !disposition.HumanAction || disposition.ReasonCode != "" || disposition.Blockers || len(disposition.BlockerEvidence) != 0 || execution.change.Reviewed {
					t.Fatalf("existing human_action park was not used: %+v", disposition)
				}
				for _, text := range []string{"auth", "first verdict undetermined", "higher-model verdict undetermined", "Whether the caller validates tenant authorization"} {
					if !strings.Contains(execution.preparedMessage, text) || !strings.Contains(disposition.FinalSummary, text) {
						t.Fatalf("completion missing %q", text)
					}
				}
			}
			if tc.blocked && (execution.change.Reviewed || execution.change.SecurityAudit == nil || execution.change.SecurityAudit.Verdict != "rework" || len(execution.change.SecurityAudit.Run.Findings) != 1) {
				t.Fatalf("unchanged retry lost blocking audit and Rework routing: %+v", execution.change)
			}
		})
	}
}

func TestNativeSecurityAuditCapturesBeyondStoredDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	isolateNativeChangeGit(t)
	for _, size := range []int{0, 300 << 10, 21 << 20, tracker.MaxChangeDiffBytes} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			directory := nativeChangeSourceRepo(t)
			gitOutput := func(args ...string) string {
				data, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory}, args...)...).Output()
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			base := strings.TrimSpace(gitOutput("rev-parse", "HEAD"))
			for path, content := range map[string]string{"large.txt": strings.Repeat("source line\n", size/11+1), "zz-late.txt": "late security change\n"} {
				if size == 0 {
					continue
				}
				if err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			nativeChangeGit(t, directory, "add", ".")
			nativeChangeGit(t, directory, "commit", "--allow-empty", "-m", "audit source")
			head := strings.TrimSpace(gitOutput("rev-parse", "HEAD"))
			var original string
			var expected [sha256.Size]byte
			if size < tracker.MaxChangeDiffBytes {
				original = gitOutput("diff", "--no-ext-diff", "--no-textconv", "--find-renames", base, head, "--")
				if size > 0 && len(original) <= size {
					t.Fatal("fixture did not cross source boundary")
				}
				expected = sha256.Sum256([]byte(original))
			}
			captured, err := workspace.GitFileDiffs(t.Context(), directory, base, tracker.MaxDiffBytes)
			if err != nil {
				t.Fatal(err)
			}
			request := tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: head}
			for _, file := range captured.Files {
				request.Files = append(request.Files, tracker.AttemptDiffFile{Path: file.Path, Status: file.Status, Patch: file.Patch, Additions: file.Additions})
			}
			if captured.Truncated {
				request.Files = tracker.StripDiffPatches(request.Files)
			}
			request.Files, _ = tracker.NormalizeDiffFiles(request.Files)
			version := tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example-org/example-project", BaseSHA: base, HeadSHA: head, PolicyID: "policy_example"}, Policy: policy.Descriptor{Gates: policy.Gates{SecurityAudit: true}}}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_example", ProjectID: "prj_example", WorkItemID: "wi_example", CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}}
			runtime := tracker.NativeRuntimeEvidence{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example"}}, Attempt: &tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: "attempt_example", PolicyID: version.PolicyID}}}
			client, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value any
				switch {
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					value = nativeCapabilities{Features: []string{tracker.NativeRuntimeEvidenceCapability}}
				case strings.HasSuffix(r.URL.Path, "/runtime"):
					value = runtime
				case strings.HasSuffix(r.URL.Path, "/changes/change_example"):
					page := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Detail: &detail, Read: &operatortool.ChangeRead{VersionID: version.ID, Sections: map[string]operatortool.ChangeReadPage{}}}
					for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
						page.Read.Sections[section] = operatortool.ChangeReadPage{Complete: true}
					}
					value = page
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			execution := &nativeExecution{claim: nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: "wi_example"}}, change: &runner.NativeChange{ChangeID: detail.Change.ID, VersionID: version.ID}, data: runtime.Attempt.NativeRunData, lastDiff: &request}
			if size == 0 {
				execution.recoveredSource = &tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: head}
			}
			execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
				return workspace.CaptureChangeSource(ctx, directory, base, head)
			})
			if size >= tracker.MaxChangeDiffBytes {
				detail.Versions[0].Policy.Gates.SecurityAudit = false
				if err := execution.retainChangeSource(t.Context(), request); err != nil {
					t.Fatalf("audit patch limit prevented source retention: %v", err)
				}
				retained := execution.retainedSource
				if retained == nil {
					t.Fatal("source bundle was not retained")
				}
				if err := retained.Source.Validate(base, head, retained.Bundle); err != nil {
					t.Fatal(err)
				}
				if retained.Patch != "" {
					t.Fatal("oversized audit patch must not be represented as complete")
				}
				if input, err := execution.SecurityAuditVersion(t.Context()); err != nil || input.Version != nil {
					t.Fatalf("disabled audit rejected retained source: version=%v error=%v", input.Version != nil, err)
				}
				detail.Versions[0].Policy.Gates.SecurityAudit = true
				if input, err := execution.SecurityAuditVersion(t.Context()); err == nil || !strings.Contains(err.Error(), "security audit retained source patch is unavailable") || input.Diff != nil {
					t.Fatalf("incomplete audit source was not refused: diff=%v error=%v", input.Diff != nil, err)
				}
				return
			}
			input, err := execution.SecurityAuditVersion(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if input.Diff == nil {
				t.Fatal("audit source missing")
			}
			snapshot, err := connector.SecurityAuditSnapshot("prj_example", connector.Issue{ID: "wi_example"}, *input.Diff)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.BaseSHA != base || snapshot.HeadSHA != head || snapshot.DiffTruncated || sha256.Sum256([]byte(snapshot.Diff)) != expected || size > 0 && !strings.Contains(snapshot.Diff, "+late security change") {
				t.Fatalf("audit lost exact source: captured=%d original=%d", len(snapshot.Diff), len(original))
			}
		})
	}
}

type reservedAuditBackend struct {
	check    func()
	calls    int
	deadline time.Time
}

func (*reservedAuditBackend) ListModels(context.Context, runner.AgentProcessRequest) ([]runner.AgentModel, error) {
	return []runner.AgentModel{{ID: "complex-model", Model: "complex-model"}}, nil
}

func (b *reservedAuditBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.check()
	b.calls++
	deadline, ok := ctx.Deadline()
	if !ok || !b.deadline.IsZero() && !deadline.Equal(b.deadline) {
		return runner.AgentTurnResult{}, errors.New("verification reset the stage deadline")
	}
	b.deadline = deadline
	if request.Resume.ThreadID != "" || request.Resume.SessionID != "" {
		return runner.AgentTurnResult{}, errors.New("verification resumed a session")
	}
	body := `{"verdict":"undetermined","summary":"Potential authorization bypass","findings":[{"id":"auth","certainty":"undetermined","body":"Potential p2 requires caller context","unresolved_fact":"Whether caller authorizes tenant access","status":"unresolved"}]}`
	if b.calls == 2 {
		if request.Model != "complex-model" {
			return runner.AgentTurnResult{}, errors.New("verification did not select complex model")
		}
		body = `{"verdict":"pass","summary":"Lead rejected","findings":[{"id":"auth","certainty":"disproved","body":"Caller authorizes tenant access","status":"rejected"}]}`
	}
	for _, event := range []runner.AgentUpdate{
		{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1 << 30, GroupID: 1 << 30, StartedAt: time.Now()}},
		{Type: runner.AgentUpdateMessageDelta, Delta: body},
	} {
		if err := update(event); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	b.check()
	return runner.AgentTurnResult{ThreadID: "audit-" + strconv.Itoa(b.calls), AuthenticationMode: "api_key"}, nil
}

func TestNativeSecurityAuditRetainsProviderReservation(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	h.admin.client.baseURL.Scheme = "https"
	report := providercapacity.Report{Provider: "openai", Backend: "audit", AccountAlias: "example", SharedAccountAlias: "example", Models: []string{"audit-model", "complex-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now()}
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, "https://native-hub.test")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"audit": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "example", DisplayName: "Audit runner", Capacity: 2, Version: "test"}
	var identity runnerauth.Identity
	if err := h.admin.client.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", runnerauth.Redemption{BackendIsolation: machine.BackendIsolation, Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version}, &identity); err != nil {
		t.Fatal(err)
	}
	file.Identity = identity
	if err := runnerauth.Save(identityPath, file); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
	if err != nil {
		t.Fatal(err)
	}
	h.native, err = client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, ProviderReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{report}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	issue := h.createInProgress(t, "Audit candidate")
	requirement := providercapacity.Requirement{Role: runner.RoleSecurityAudit, Backend: "audit", Model: "audit-model"}
	request := orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"In Progress"}, ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
		return requirement, nil
	}}
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 || candidates[0].ID != issue.ID {
		t.Fatalf("audit claim: %v, %v", candidates, err)
	}
	execution := &nativeExecution{claim: h.scheduler.nativeClaims[issue.ID]}
	reservation := execution.ProviderCapacity()
	if reservation == nil {
		t.Fatal("audit has no provider reservation")
	}
	lease := execution.Recovery().Lease
	competitor := h.createInProgress(t, "Waiting audit candidate")
	competingIssue, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(competitor.ID))
	if err != nil {
		t.Fatal(err)
	}
	claims, releases, probeClaims := 0, 0, 0
	transport := h.failChanges.next
	h.failChanges.next = executionRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/claims") {
			claims++
		}
		if strings.HasSuffix(req.URL.Path, "/release") {
			releases++
		}
		return transport.RoundTrip(req)
	})
	competingClaim := tracker.NativeClaim{MachineID: h.scheduler.machine.ID, SessionID: "competing-audit", TTLSeconds: 90, ProtocolMajor: 2, PolicyID: h.descriptor.ID, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability, tracker.NativeProviderCapacityCapability}, ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: tracker.NativeWorkItemID(competitor.ID), Revision: competingIssue.Revision, Requirement: requirement}}}
	check := func() {
		t.Helper()
		if execution.ProviderCapacity() != reservation || execution.Recovery().Lease.ID != lease.ID {
			t.Fatal("audit replaced original reservation")
		}
		if claims != probeClaims || releases != 0 || len(h.scheduler.nativeClaims) != 1 {
			t.Fatalf("audit changed capacity ownership: claims=%d releases=%d held=%d", claims, releases, len(h.scheduler.nativeClaims))
		}
		blocked, err := h.native.Claim(t.Context(), competingClaim)
		var refusal *APIError
		if !errors.As(err, &refusal) || refusal.Code != "provider_capacity" || blocked.ID != "" {
			t.Fatalf("reserved slot became available during audit: %+v, %v", blocked, err)
		}
		probeClaims++
	}
	backend := &reservedAuditBackend{check: check}
	cfg := config.Default()
	cfg.Agents.ModelSelection.Enabled = new(false)
	cfg.Agents.ModelSelection.ComplexModel = new("complex-model")
	cfg.Agents.Backends = []config.AgentBackend{{ID: "audit", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-audit"}}
	cfg.Agents.Routes = []config.AgentRoute{{Name: "audit", Role: runner.RoleSecurityAudit, Backend: "audit", Model: "audit-model", Default: true}}
	sessions, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sessions.Close(); err != nil {
			t.Error(err)
		}
	})
	agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: cfg}, Workspace: &providerPreviewWorkspace{}, AgentBackends: map[string]runner.AgentBackend{"audit": backend}, Store: sessions, SecurityAuditRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Audit(t.Context(), runner.SecurityAuditRequest{Issue: candidates[0], Snapshot: securityaudit.Snapshot{ProjectID: string(h.project), IssueID: issue.ID, Repository: "owner/repository", BaseSHA: "base", HeadSHA: "head", Diff: "+change"}})
	if err != nil || backend.calls != 2 || result.Verification == nil {
		t.Fatalf("audit escalation: calls=%d result=%+v error=%v", backend.calls, result, err)
	}
	check()
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "dispatch_deferred"); err != nil {
		t.Fatal(err)
	}
	if releases != 1 || len(h.scheduler.nativeClaims) != 0 {
		t.Fatalf("stage completion did not release reservation: releases=%d held=%d", releases, len(h.scheduler.nativeClaims))
	}
	next, err := h.native.Claim(t.Context(), competingClaim)
	if err != nil || next.WorkItemID != tracker.NativeWorkItemID(competitor.ID) || next.ProviderReservation == nil {
		t.Fatalf("released slot was not reusable: %+v, %v", next, err)
	}
	if err := h.native.Release(t.Context(), next, "released"); err != nil {
		t.Fatal(err)
	}
}
