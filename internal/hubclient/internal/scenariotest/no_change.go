package scenariotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNoChangeGateFailureHistory(t *testing.T, factories Constructors) {
	for _, test := range []struct {
		name     string
		failures int
	}{
		{name: "two failures then pass", failures: 2},
		{name: "failure history requires continuation", failures: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPlannerHub(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, factories)
			issue := h.createInProgress(t, "Verify existing behavior")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
			e.SetRepository(nativeChangeRepository)
			ctx := t.Context()
			base, tree := nativeChangeFixtureHead("b"), strings.Repeat("d", 40)
			e.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: base}, true
			})
			if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := e.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: base, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			resolved, err := config.ResolveNativePolicy(h.descriptor)
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := resolved.Workflow()
			if err != nil {
				t.Fatal(err)
			}
			workflow.Config.Policy = h.descriptor
			expected, err := progresscheck.FreezeChecklist(issue.Description, workflow.Config)
			if err != nil {
				t.Fatal(err)
			}
			command := gate.Effective(workflow.Config.Gate).Run
			if command == "" {
				t.Fatal("fixture needs a project gate")
			}
			legacy := expected
			legacy.Checks = slices.Clone(expected.Checks)
			gateIndex := slices.IndexFunc(legacy.Checks, func(check tracker.ExecutionCheck) bool { return check.Kind == tracker.CheckProjectGate })
			legacy.Checks[gateIndex].Text = "Run `" + command + "`."
			legacy.Checks = slices.Insert(legacy.Checks, gateIndex, tracker.ExecutionCheck{Kind: tracker.CheckVerification, Text: "Run the project checks.", Status: tracker.CheckNeedsReview})
			for i := range legacy.Checks {
				legacy.Checks[i].Ordinal = i + 1
			}
			freezer := e.Lifecycle.(interface {
				FreezeExecutionChecklist(context.Context, tracker.ExecutionChecklist) error
			})
			if err := freezer.FreezeExecutionChecklist(ctx, legacy); err != nil {
				t.Fatalf("Hub rejected older runner's checklist shape: %v", err)
			}
			attemptID := e.CompletionData().AttemptID
			item := tracker.NativeWorkItemID(issue.ID)
			stored, err := h.native.ExecutionChecklist(ctx, item, attemptID)
			if err != nil || stored == nil || stored.Revision != expected.Revision || !slices.Equal(stored.Checks, expected.Checks) {
				t.Fatalf("Hub checklist = %+v, want canonical rows %+v, error = %v", stored, expected.Checks, err)
			}

			receipt := gate.CommandResult{Command: command, HeadSHA: base, TreeSHA: tree, ExitCode: 1, Stage: gate.StageSourceFinalization, DurationNS: int64(time.Millisecond)}
			started := time.Now().UTC().Add(-time.Minute)
			record := func(index int) {
				t.Helper()
				receipt.StartedAt = started.Add(time.Duration(index) * time.Second)
				receipt.FinishedAt = receipt.StartedAt.Add(time.Millisecond)
				receipt.ReceiptID = receipt.PipelineTiming("finalization").ID()
				if err := e.RecordSourceValidation(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			}
			want := make(map[string]string)
			for attempt := 0; attempt < test.failures; attempt++ {
				receipt.Output = fmt.Sprintf("TestReport%02d failed", attempt)
				record(attempt)
				want[receipt.ReceiptID] = receipt.Output
			}
			receipt.ExitCode, receipt.Output = 0, "PASS"
			record(test.failures)
			message := "```detent-status\nschema: 1\nstatus: complete\nfields:\n  completion_kind: no_change_needed\n  completion_verified_sha: " + base + "\n  completion_evidence: Target regression passed\n```"
			if err := e.PrepareFinish(ctx, "succeeded", message, nil); err != nil {
				t.Fatal(err)
			}
			for _, read := range []func(context.Context) (runner.NativeValidation, error){e.ReviewVersion, e.ValidatorVersion} {
				input, err := read(ctx)
				if err != nil || input.NoChange == nil || input.Version == nil || input.Version.Validation == nil || input.Version.Validation.ReceiptID != receipt.ReceiptID || len(input.Commands) != 0 {
					t.Fatalf("first no-change review input = %+v, error = %v", input, err)
				}
				got := slices.Clone(input.ProjectGateFailures)
				continuation := input.GateFailureContinuation
				if (continuation != nil) != (test.failures > 8) {
					t.Fatalf("initial failure history has %d receipts, continuation = %+v", len(got), continuation)
				}
				if continuation != nil {
					args, err := json.Marshal(operatortool.WorkReadRequest{ProjectID: string(h.native.ProjectID()), Reference: issue.ID, NativeAttemptID: continuation.AttemptID, Cursor: continuation.After})
					if err != nil {
						t.Fatal(err)
					}
					result, err := h.native.ReadAgentTool(ctx, runner.AgentToolCall{Name: operatortool.WorkAttemptReceipt, Arguments: args}, time.Now())
					var page operatortool.WorkReadResult[tracker.ProjectGateFailurePage]
					if err != nil || json.Unmarshal(result.Content, &page) != nil || page.Data.Incomplete || page.Data.NextCursor != "" || len(page.Data.Failures) != 1 {
						t.Fatalf("continuation returned %s, error = %v", result.Content, err)
					}
					got = append(got, page.Data.Failures...)
				}
				if len(got) != len(want) {
					t.Fatalf("received %d failed receipts, want %d", len(got), len(want))
				}
				seen := map[string]bool{}
				for _, failure := range got {
					if failure.Output != want[failure.ReceiptID] || seen[failure.ReceiptID] {
						t.Fatalf("failure evidence = %+v", failure)
					}
					seen[failure.ReceiptID] = true
				}
			}
			if changes, err := h.admin.Changes(ctx, item); err != nil || len(changes) != 0 {
				t.Fatalf("history review required a published Change: %+v, error = %v", changes, err)
			}
		})
	}
}

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
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement, DeferExecutionFinish: test.restart})
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
