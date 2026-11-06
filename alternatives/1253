package scenariotest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNoChangeCompletion(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	for _, reported := range []string{"current", "stale", "empty Change already integrated", "restored without verification target"} {
		t.Run(reported, func(t *testing.T) {
			h := newPlannerHub(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, factories)
			issue := h.createInProgress(t, "Verify existing behavior")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
			e.SetRepository(nativeChangeRepository)
			base := nativeChangeFixtureHead("b")
			e.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: base}, true
			})
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: base, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			sha := base
			if reported == "stale" {
				sha = strings.Repeat("a", 40)
			}
			if reported == "empty Change already integrated" {
				item := tracker.NativeWorkItemID(issue.ID)
				change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Already integrated behavior"})
				if err != nil {
					t.Fatal(err)
				}
				capture := nativeChangeSourceFixture(t.Context(), t, nativeChangeFixtureHead("a"), nativeChangeFixtureHead("c"))
				capture.Source.DiffSHA256 = tracker.ChangeSourceDigest(nil)
				version := h.publishCaptured(t, item, change.ID, nativeChangeFixtureHead("c"), &capture)
				if version.Source == nil || version.Source.DiffSHA256 != tracker.ChangeSourceDigest(nil) {
					t.Fatal("fixture must have an empty published source delta")
				}
				if err := e.RecordSourceValidation(t.Context(), gate.CommandResult{Command: "true", HeadSHA: base, TreeSHA: strings.Repeat("d", 40)}); err != nil {
					t.Fatal(err)
				}
			}
			message := "```detent-status\nschema: 1\nstatus: complete\nfields:\n  completion_kind: no_change_needed\n  completion_verified_sha: " + sha + "\n  completion_evidence: Target regression passed\n```"
			if err := e.PrepareFinish(t.Context(), "succeeded", message, nil); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name string
				read func(context.Context) (runner.NativeValidation, error)
			}{{"review", e.ReviewVersion}, {"validator", e.ValidatorVersion}, {"security_audit", e.SecurityAuditVersion}} {
				input, err := read.read(t.Context())
				if err != nil || input.Version == nil || input.Version.BaseSHA != base || input.Version.HeadSHA != base || input.Diff == nil || len(input.Diff.Files) != 0 {
					t.Fatalf("%s did not receive current-base evidence: %+v, %v", read.name, input, err)
				}
			}
			change := e.NativeChange()
			if change == nil || change.Changed || change.ChangeID != "" || change.VersionID != "" || change.Landing != nil || change.Reviewed {
				t.Fatalf("unreviewed no-change report created a deliverable or approval: %+v", change)
			}
			input, err := e.ReviewVersion(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if reported == "empty Change already integrated" {
				result := gate.ValidatorResult{VersionID: input.Version.ID, Repository: input.Version.Repository, BaseSHA: base, HeadSHA: base, Verdict: gate.ValidatorVerdictPass, Score: 1, SessionID: 41, Submitted: true, Summary: "Acceptance criteria hold on the current base"}
				if err := e.RecordReview(t.Context(), result); err != nil {
					t.Fatal(err)
				}
				result.SessionID = 42
				if err := e.RecordValidator(t.Context(), result); err != nil {
					t.Fatal(err)
				}
				if !e.NativeChange().NoChangeAccepted(gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}}) {
					t.Fatal("approved empty Change did not complete through no-change acceptance")
				}
				return
			}
			result := gate.ValidatorResult{VersionID: input.Version.ID, Repository: input.Version.Repository, BaseSHA: base, HeadSHA: base, Verdict: gate.ValidatorVerdictRework, SessionID: 42, Submitted: true, Summary: "Acceptance regression is missing"}
			if err := e.RecordReview(t.Context(), result); err != nil {
				t.Fatal(err)
			}
			if got := e.NativeChange(); got.ImplementationReview == nil || got.ImplementationReview.Summary != result.Summary || got.Reviewed {
				t.Fatalf("rejection feedback was lost: %+v", got)
			}
			if reported == "restored without verification target" {
				e.RetainCompletion()
				change, err := e.SourceReview(t.Context())
				if err != nil || change.VersionError == "" || change.ImplementationReview != nil || change.Validator != nil || change.SecurityAudit != nil {
					t.Fatalf("restored completion did not request fresh source verification: %+v, %v", change, err)
				}
				return
			}
			if _, err := e.SourceReview(t.Context()); !errors.Is(err, runner.ErrValidatorInfrastructure) {
				t.Fatalf("unavailable source verification was not instance-owned: %v", err)
			}
		})
	}
}

type noChangeEvidenceAgent struct{ committingAgent }

func (a *noChangeEvidenceAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return a.RunTurnWithTools(ctx, request, nil, nil, update)
}

func (a *noChangeEvidenceAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handler runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if strings.Contains(request.Prompt, "Detent's independent security auditor") {
		a.calls++
		if request.Workspace == a.workspace || request.Resume.ThreadID != "" {
			return runner.AgentTurnResult{}, errors.New("audit reused implementation workspace or session")
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1 << 30, GroupID: 1 << 30, StartedAt: time.Now()}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: `{"verdict":"pass","summary":"No security changes; inspected current base evidence","findings":[]}`}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-audit", TurnID: "audit-turn", AuthenticationMode: "api_key"}, nil
	}
	return a.committingAgent.RunTurnWithTools(ctx, request, tools, handler, update)
}

func RunNoChangeRequiredGates(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name                string
		audit               bool
		ssh                 bool
		advance             bool
		advanceDuringReview bool
		restart             bool
		advanceAfterRun     bool
	}{
		{name: "review and validation"},
		{name: "base advances during implementation", advance: true},
		{name: "base advances during review", advanceDuringReview: true},
		{name: "security audit", audit: true},
		{name: "SSH security audit", audit: true, ssh: true},
		{name: "restart deferred completion", audit: true, restart: true},
		{name: "SSH base moves after exit", ssh: true, advanceAfterRun: true},
		{name: "base moves during restart", restart: true, advanceAfterRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPlannerHub(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, factories)
			next := h.descriptor
			next.Gates.Validator, next.Gates.SecurityAudit, next.Gates.AutomatedReview = true, test.audit, gate.AutomatedReviewRequired
			next = next.WithID()
			if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
				t.Fatal(err)
			}
			h.descriptor = next
			issue := h.createInProgress(t, "Complete the requested work")
			candidate := h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			source := nativeChangeSourceRepo(t)
			baseOutput, err := exec.CommandContext(t.Context(), "git", "-C", source, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSpace(string(baseOutput))
			remote := filepath.Join(t.TempDir(), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			gitConfig := filepath.Join(t.TempDir(), "gitconfig")
			t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
			nativeChangeGit(t, source, "config", "--global", "url."+remote+".insteadOf", nativeChangeRepository)
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
			if err != nil {
				t.Fatal(err)
			}
			defer sessions.Close()
			message := "```detent-status\nschema: 1\nstatus: no_change_needed\nfields:\n  completion_verified_sha: " + strings.Repeat("a", 40) + "\n  completion_evidence: Inspect README.md and run git status --porcelain on current base\n```"
			provider := &noChangeEvidenceAgent{committingAgent: committingAgent{finalMessage: message, validator: "pass"}}
			if test.advance || test.advanceDuringReview {
				advance := func() {
					if err := os.WriteFile(filepath.Join(source, "LATEST.md"), []byte("current base\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, source, "add", "LATEST.md")
					nativeChangeGit(t, source, "commit", "-m", "advance current base")
					nativeChangeGit(t, source, "push", "origin", "main")
					output, err := exec.CommandContext(t.Context(), "git", "-C", source, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					base = strings.TrimSpace(string(output))
				}
				if test.advance {
					provider.duringTurn = advance
				} else {
					provider.duringValidation = func(runner.AgentTurnRequest) { advance() }
				}
			}

			gateCfg := gate.Config{Run: "git status --porcelain", SecurityAudit: gate.SecurityAuditConfig{Enabled: test.audit}, AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}}
			agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Store: sessions, ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Agent: config.Agent{MaxTurns: 5}, Gate: gateCfg}, Prompt: "Complete the issue"}, Workspace: backend, AgentBackend: provider, ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) { return 0, nil }})
			if err != nil {
				t.Fatal(err)
			}
			runExecution := execution
			closePeers := func() {}
			if test.ssh {
				runExecution, closePeers = nativeSSHExecution(t, t.Context(), execution, t.TempDir())
				defer closePeers()
			}
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			if err != nil {
				t.Fatal(err)
			}
			change := result.NativeChange
			if test.advanceDuringReview {
				if change == nil || change.NoChangeAccepted(gateCfg) || !strings.Contains(change.VersionError, "base advanced") {
					t.Fatalf("stale base approval was accepted: %+v", change)
				}
				return
			}
			if change == nil || !change.NoChangeAccepted(gateCfg) || change.HeadSHA != base || change.ChangeID != "" || change.VersionID != "" || change.Landing != nil || provider.calls != 3+boolCount(test.audit) {
				t.Fatalf("no-change gates did not accept current base without publication: change=%+v calls=%d", change, provider.calls)
			}
			owner := execution.(*ownedExecution)
			if owner.CompletionSnapshot().WorktreeHead != base {
				t.Fatalf("no-change completion created a commit instead of verifying base: head=%s base=%s", owner.CompletionSnapshot().WorktreeHead, base)
			}
			seen := map[int64]bool{}
			for _, result := range []*gate.ValidatorResult{change.ImplementationReview, change.Validator} {
				if result == nil || result.SessionID == 0 || seen[result.SessionID] || !strings.Contains(result.Repository, "example") {
					t.Fatalf("review session was not independent: %+v", result)
				}
				seen[result.SessionID] = true
			}
			if test.audit && (change.SecurityAudit == nil || seen[change.SecurityAudit.SessionID]) {
				t.Fatalf("audit did not have an independent session: %+v", change.SecurityAudit)
			}
			if len(owner.CompletionData().Steps) < 4 {
				t.Fatalf("gate receipts were not retained: %+v", owner.CompletionData().Steps)
			}
			current, err := h.native.CurrentChange(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || current.Change.ID != "" {
				t.Fatalf("no-change run published a Change: %+v, %v", current, err)
			}
			closePeers()
			if !test.ssh && !test.restart {
				return
			}
			if test.restart {
				client := owner.claim.source.NativeClient().Transport()
				transport := fixtureClient(client).requests.Load()
				transitionAttempts := 0
				fixtureClient(client).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/workflow") {
						transitionAttempts++
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("completion unavailable")), Request: request}, nil
					}
					return transport.RoundTrip(request)
				}))
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Done"); err == nil || transitionAttempts == 0 {
					t.Fatalf("completion transition was not deferred: requests=%d error=%v", transitionAttempts, err)
				}
				fixtureClient(client).requests.Store(transport)
				owner.RetainCompletion()
				saved := owner.CompletionState()
				h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: fixtureSchedulerMachine(h.scheduler), HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				owner = h.scheduler.RunExecution(issue.ID).(*ownedExecution)
			}
			if test.advanceAfterRun {
				if err := os.WriteFile(filepath.Join(source, "AFTER.md"), []byte("advanced after worker exit\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, source, "add", "AFTER.md")
				nativeChangeGit(t, source, "commit", "-m", "advance after worker exit")
				nativeChangeGit(t, source, "push", "origin", "main")
			}
			verified, err := owner.SourceReview(t.Context())
			if err != nil {
				t.Fatalf("completion could not verify source after worker exit or restart: %v", err)
			}
			if test.advanceAfterRun {
				if verified.NoChangeAccepted(gateCfg) || !strings.Contains(verified.VersionError, "base advanced") {
					t.Fatalf("completion accepted a moved base: %+v", verified)
				}
				return
			}
			if !verified.NoChangeAccepted(gateCfg) {
				t.Fatalf("completion lost accepted gate receipts: %+v", verified)
			}
			if test.restart {
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Done"); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				if err := owner.Validate(t.Context()); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("restored completion retained execution authority: %v", err)
				}
				if state := h.state(t, issue.ID); state != "Done" || provider.calls != 3+boolCount(test.audit) {
					t.Fatalf("restored completion did not finish without another worker turn: state=%s calls=%d", state, provider.calls)
				}
				return
			}
			runNativeCompletionBackend(t, h, &nativeCompletionRunner{Runner: agent, prepared: &result, gate: gateCfg, completed: make(chan nativeCompletionRun, 1)}, runner.RunRequest{Execution: owner, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement}, "Done")
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
