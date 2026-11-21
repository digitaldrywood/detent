package scenariotest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNoChangeCompletion(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	for _, reported := range []string{"current", "reviewer-only project gate", "completed review before projection", "externally retained review", "stale", "empty Change already integrated", "restored without verification target"} {
		t.Run(reported, func(t *testing.T) {
			archived := reported == "externally retained review"
			historical := reported == "completed review before projection"
			hubConfig := hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Conversation: &conversations.ConversationConfig{Enabled: true}}
			var retained []byte
			var retainedMu sync.Mutex
			if archived {
				archiveContent := newHubContentStore()
				hubConfig.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
					if request.Attachment == nil {
						return archiveContent(ctx, request)
					}
					retainedMu.Lock()
					defer retainedMu.Unlock()
					return bytes.Clone(retained), nil
				}
			}
			h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, true, false, hubConfig, factories)
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
				change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: api.NativeMutationKey(), Title: "Already integrated behavior"})
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
			if reported == "reviewer-only project gate" || historical || archived {
				cfg := config.Default()
				cfg.Policy = h.descriptor
				list, err := progresscheck.FreezeChecklist(issue.Description, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if !historical {
					if err := e.FreezeExecutionChecklist(t.Context(), list); err != nil {
						t.Fatal(err)
					}
				}
				command := gate.CommandResult{Command: gate.Effective(cfg.Gate).Run, HeadSHA: base, TreeSHA: strings.Repeat("d", 40), Execution: "executed"}
				command.StartedAt = time.Now().UTC().Add(-time.Second)
				command.FinishedAt = command.StartedAt.Add(time.Second)
				command.ReceiptID = command.PipelineTiming("validator_criteria").ID()
				result := gate.ValidatorResult{VersionID: input.Version.ID, Repository: input.Version.Repository, BaseSHA: base, HeadSHA: base, Verdict: gate.ValidatorVerdictPass, Score: 1, SessionID: 41, Submitted: true, Summary: "Acceptance criteria hold on the current base", Commands: []gate.CommandResult{command}}
				if archived {
					result.Summary = strings.Repeat("Verified behavior. ", tracker.NativeStepReceiptBytes/10)
					attempt := e.CompletionData().AttemptID
					requests := fixtureClient(e.claim.source.NativeClient().Transport()).requests
					previous := requests.Load()
					requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/attempts/"+attempt+"/evidence") {
							return previous.RoundTrip(request)
						}
						raw, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						metadata := attachment.Metadata{ID: "att_" + strings.Repeat("a", 32), ProjectID: string(h.project), WorkItemID: issue.ID,
							Uploader: "example_admin", Name: "execution-step.json", ContentType: "application/json", SHA256: artifact.Digest(raw), Size: int64(len(raw)), CreatedAt: time.Now().UTC()}
						metadata.Reference = metadata.Markdown(string(h.organization))
						h.seed(func(w tenantstore.Writer) error {
							return w.CreateAttachment(request.Context(), attachmentstore.Record{OrganizationID: h.organization, Metadata: metadata, EvidenceAttemptID: attempt, Kind: "attachment"})
						})
						retainedMu.Lock()
						retained = raw
						retainedMu.Unlock()
						encoded, err := json.Marshal(metadata)
						return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded)), Request: request}, err
					}))
					t.Cleanup(func() { requests.Store(previous) })
				}
				if err := e.RecordReview(t.Context(), result); err != nil {
					t.Fatal(err)
				}
				if err := e.RecordStep(t.Context(), tracker.NativeStepReceipt{Step: "review", CompletedAt: command.FinishedAt, Validator: &result,
					Source: tracker.NativeCheckpoint{HeadSHA: base, WorkspaceDigest: "base"}, Change: tracker.NativeChangeReference{HeadSHA: base, VersionID: input.Version.ID}}); err != nil {
					t.Fatal(err)
				}
				if change.NoChange.Validation != nil || !e.NativeChange().NoChangeAccepted(gate.Config{Run: command.Command, AutomatedReview: gate.AutomatedReviewRequired}) {
					t.Fatal("no-change completion did not accept the reviewer-only gate receipt")
				}
				if historical {
					if err := e.FreezeExecutionChecklist(t.Context(), list); err != nil {
						t.Fatal(err)
					}
				}
				if archived && e.CompletionData().Steps[len(e.CompletionData().Steps)-1].PayloadAttachment == nil {
					t.Fatal("review payload was not retained externally")
				}
				if archived || historical {
					if err := e.Finish(t.Context(), "succeeded"); err != nil {
						t.Fatal(err)
					}
					if e.CompletionData().Outcome != "succeeded" {
						t.Fatal("no-change attempt did not finish successfully")
					}
				}
				stored, err := h.admin.ExecutionChecklist(t.Context(), tracker.NativeWorkItemID(issue.ID), e.CompletionData().AttemptID)
				if err != nil || stored == nil || !slices.ContainsFunc(stored.Checks, func(check tracker.ExecutionCheck) bool {
					return check.Kind == tracker.CheckProjectGate && check.Status == tracker.CheckPassed && check.Receipt == command.ReceiptID && check.Head == base && check.Tree == command.TreeSHA
				}) {
					t.Fatalf("no-change checklist did not report the reviewer-only gate receipt: %+v, error = %v", stored, err)
				}
				return
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
				if err := e.PrepareSourceIteration(t.Context(), tracker.NativeChangeReference{HeadSHA: base}); err != nil {
					t.Fatal(err)
				}
				if !e.OwnsWork() {
					t.Fatal("source iteration discarded explicit handoff ownership")
				}
				return
			}
			if _, err := e.SourceReview(t.Context()); !errors.Is(err, runner.ErrValidatorInfrastructure) {
				t.Fatalf("unavailable source verification was not instance-owned: %v", err)
			}
			if err := e.PrepareSourceIteration(t.Context(), tracker.NativeChangeReference{HeadSHA: base}); err != nil {
				t.Fatal(err)
			}
			if e.OwnsWork() {
				t.Fatal("source iteration retained an abandoned prepared publisher")
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
			gateCfg := gate.Config{Run: "git status --porcelain", SecurityAudit: gate.SecurityAuditConfig{Enabled: test.audit}, AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}}
			workflow := config.Workflow{Config: config.Default(), Prompt: "Complete the issue"}
			workflow.Config.Agent.MaxTurns = 5
			workflow.Config.Gate = gateCfg
			workflow.Config.Tracker.Kind, workflow.Config.Tracker.Repository = config.TrackerHubNative, nativeChangeRepository
			next, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			workflow.Config.Policy = next
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
			h.configureCloneURL(remote)
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
			sessions := api.NewRunnerSessions(h.native)
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

			agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Store: sessions, ProjectID: "local", Workflow: workflow, Workspace: backend, AgentBackend: provider, ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) { return 0, nil }})
			if err != nil {
				t.Fatal(err)
			}
			runExecution := execution
			closePeers := func() {}
			if test.ssh {
				runExecution, closePeers = nativeSSHExecution(t, t.Context(), execution, t.TempDir())
				defer closePeers()
			}
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement, DeferExecutionFinish: test.restart})
			if err != nil {
				t.Fatal(err)
			}
			owner := execution.(*ownedExecution)
			checklist, err := h.admin.ExecutionChecklist(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.CompletionData().AttemptID)
			if err != nil || checklist == nil || checklist.PolicyID != next.ID || checklist.FrozenAt.IsZero() {
				t.Fatalf("runner did not report the frozen checklist: %+v, %v", checklist, err)
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
			item := tracker.NativeWorkItemID(issue.ID)
			verdict, err := h.native.LatestRoundVerdict(t.Context(), item)
			if err != nil || verdict != nil {
				t.Fatalf("no-change verdict was published before authoritative acceptance: %+v, %v", verdict, err)
			}
			if test.restart {
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Done"); err != nil {
					t.Fatal(err)
				}
				at := time.Now().UTC()
				if err := owner.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: at, Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: true, ObservedAt: at}}); err != nil {
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
			} else {
				runNativeCompletionBackend(t, h, &nativeCompletionRunner{Runner: agent, prepared: &result, gate: gateCfg, completed: make(chan nativeCompletionRun, 1)}, runner.RunRequest{Execution: owner, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement}, "Done")
			}
			verdict, err = h.native.LatestRoundVerdict(t.Context(), item)
			if err != nil || verdict == nil || verdict.Action != progresscheck.RoundDone || verdict.Disposition != progresscheck.NoChangeDisposition || verdict.HeadSHA != base || verdict.AttemptID != owner.CompletionData().AttemptID || verdict.LeaseID != owner.Recovery().Lease.ID {
				t.Fatalf("no-change completion has no verified verdict for the original claim: %+v, %v", verdict, err)
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
