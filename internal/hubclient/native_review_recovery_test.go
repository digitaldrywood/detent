package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type recoveringReviewAgent struct {
	committingAgent
	failStage       string
	available       atomic.Bool
	sourceCalls     atomic.Int32
	securityCalls   atomic.Int32
	validatorCalls  atomic.Int32
	malformed       bool
	securityVerdict string
}

func (a *recoveringReviewAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	stage := ""
	switch {
	case strings.Contains(request.Prompt, "independent Detent implementation reviewer"):
		stage = "source"
		a.sourceCalls.Add(1)
	case strings.Contains(request.Prompt, "Detent validator-agent"):
		stage = "validator"
		a.validatorCalls.Add(1)
	case request.ReadOnly:
		stage = "security"
		a.securityCalls.Add(1)
	}
	if stage == a.failStage && !a.available.Load() {
		return runner.AgentTurnResult{}, errors.New("temporary provider startup failure")
	}
	if stage == "validator" && a.malformed {
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "invalid validator decision"}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "validator-thread", TurnID: "validator-turn"}, nil
	}
	if stage == "security" {
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: time.Now()}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		output := `{"verdict":"pass","summary":"Reviewed immutable source","findings":[]}`
		if a.securityVerdict == "rework" {
			output = `{"verdict":"fail","summary":"Authorization bypass","findings":[{"id":"auth","severity":"p1","certainty":"confirmed","body":"Authorization bypass","path":"source.go"}]}`
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: output}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "security-thread", TurnID: "security-turn", AuthenticationMode: "api_key"}, nil
	}
	return a.committingAgent.RunTurn(ctx, request, update)
}

func (a *recoveringReviewAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, _ []runner.AgentTool, _ runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return a.RunTurn(ctx, request, update)
}

func TestNativeReviewProducerRecoversAfterTemporaryFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name, stage, verdict string
		refusal              string
		publicationField     string
		restore, malformed   bool
		hostHandoff, stopped bool
	}{
		{name: "healthy"},
		{name: "tool-directed host publication handoff", hostHandoff: true},
		{name: "source", stage: "source"},
		{name: "security", stage: "security"},
		{name: "security reviewer version", stage: "security", publicationField: "run.reviewer_version"},
		{name: "security reviewer digest", stage: "security", publicationField: "run.reviewer_digest"},
		{name: "security source rejection", stage: "security", publicationField: "run.head_sha"},
		{name: "security findings", stage: "security", verdict: "rework"},
		{name: "validator", stage: "validator"},
		{name: "stopped published source", stage: "validator", stopped: true},
		{name: "restored security", stage: "security", restore: true},
		{name: "source findings", stage: "source", verdict: "rework"},
		{name: "acceptance findings", stage: "validator", verdict: "rework"},
		{name: "malformed acceptance", stage: "validator", malformed: true},
		{name: "revoked owner", stage: "source", refusal: "lease"},
		{name: "replaced source", stage: "source", refusal: "source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Blocked", "Merging", "Rework"}},
				{Name: "In Review", Transitions: []string{"In Progress"}},
				{Name: "Blocked", Transitions: []string{"Todo"}},
				{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "In Review"}},
				{Name: "Done", Terminal: true},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Merging"}},
			}, true)
			gateCommand := "true"
			gateMarker := filepath.Join(t.TempDir(), "source-gate")
			if test.hostHandoff || test.name == "healthy" {
				gateCommand = "printf gate >> '" + strings.ReplaceAll(gateMarker, "'", "'\"'\"'") + "'"
			}
			previous := h.descriptor.ID
			h.descriptor.Gates.AutomatedReview = gate.AutomatedReviewRequired
			h.descriptor.Gates.Validator = true
			h.descriptor.Gates.SecurityAudit = true
			h.descriptor = h.descriptor.WithID()
			if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
				t.Fatal(err)
			}
			issue := h.createInProgress(t, "Recover configured source reviews")
			current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			body := current.Body + issueContractTestSections
			if _, err := h.admin.UpdateIssue(t.Context(), current.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, Body: &body}); err != nil {
				t.Fatal(err)
			}
			confirmIssueContract(t, h.admin, issue.ID)
			source := nativeChangeSourceRepo(t)
			remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			runtimeStore, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := runtimeStore.Close(); err != nil {
					t.Error(err)
				}
			})
			provider := &recoveringReviewAgent{committingAgent: committingAgent{staged: true, complete: true, validator: "pass"}, failStage: test.stage, malformed: test.malformed}
			var rejectedVersion tracker.ChangeVersion
			var publicationRefusals atomic.Int32
			if test.publicationField != "" {
				provider.failStage = "publication"
				transport := h.native.client.httpClient.Transport
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/reviews") && !provider.available.Load() {
						var review tracker.ReviewChange
						if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
							return nil, err
						}
						if review.SecurityAudit != nil {
							switch test.publicationField {
							case "run.reviewer_version":
								review.SecurityAudit.Run.ReviewerVersion = "old"
							case "run.reviewer_digest":
								review.SecurityAudit.Run.ReviewerDigest = "old"
							case "run.head_sha":
								review.SecurityAudit.Run.HeadSHA = strings.Repeat("a", 40)
							}
						}
						raw, err := json.Marshal(review)
						if err != nil {
							return nil, err
						}
						if err := request.Body.Close(); err != nil {
							return nil, err
						}
						request.Body = io.NopCloser(bytes.NewReader(raw))
						request.ContentLength = int64(len(raw))
						response, err := transport.RoundTrip(request)
						if err != nil {
							return nil, err
						}
						if response.StatusCode == http.StatusUnprocessableEntity {
							publicationRefusals.Add(1)
							if test.publicationField == "run.reviewer_digest" {
								data, err := io.ReadAll(response.Body)
								if err != nil {
									return nil, err
								}
								if err := response.Body.Close(); err != nil {
									return nil, err
								}
								data = bytes.ReplaceAll(data, []byte("run.reviewer_version"), []byte("run.reviewer_digest"))
								response.Body = io.NopCloser(bytes.NewReader(data))
								response.ContentLength = int64(len(data))
							}
						}
						return response, nil
					}
					return transport.RoundTrip(request)
				})
			}
			var previousVersion tracker.ChangeVersion
			if test.hostHandoff {
				info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
				if err != nil {
					t.Fatal(err)
				}
				readHead := func() string {
					output, err := exec.CommandContext(t.Context(), "git", "-C", info.Path, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSpace(string(output))
				}
				base := readHead()
				if err := os.WriteFile(filepath.Join(info.Path, "PRESERVED.md"), []byte("published source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, info.Path, "add", "PRESERVED.md")
				nativeChangeGit(t, info.Path, "commit", "-m", "published source")
				head := readHead()
				capture, err := workspace.CaptureChangeSource(t.Context(), info.Path, base, head)
				if err != nil {
					t.Fatal(err)
				}
				change, err := h.admin.CreateChange(t.Context(), current.WorkItemID, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
				if err != nil {
					t.Fatal(err)
				}
				previousVersion = h.publishCaptured(t, current.WorkItemID, change.ID, head, &capture, "", base)
				detail, err := h.admin.currentChange(t.Context(), current.WorkItemID)
				if err != nil || detail.Summary.Status != "needs_evidence" || len(detail.Reviews) != 0 {
					t.Fatalf("old version unexpectedly reviewed: %+v, %v", detail, err)
				}
				currentIssue, err := h.admin.Issue(t.Context(), current.WorkItemID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: "Rework", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				provider.finalMessage = "Source complete; published review is pending.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: host_publication_pending\nblockers:\n  - ref: instance:tool\n    reason: Published version needs host review\nhuman_action: null\n```"
			}
			provider.available.Store(test.stage == "")
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(logs.String())
				}
			})
			if test.verdict != "" {
				if test.stage == "source" {
					provider.sourceVerdict = test.verdict
				} else if test.stage == "security" {
					provider.securityVerdict = test.verdict
				} else {
					provider.validator = test.verdict
				}
			}
			agent, err := runner.NewRunner(runner.Dependencies{Logger: logger, Store: runtimeStore, ProjectID: "local", Workspace: backend, AgentBackend: provider, Workflow: config.Workflow{Config: config.Config{Policy: h.descriptor, Tracker: config.Tracker{Kind: config.TrackerHubNative}, Gate: gate.Config{Run: gateCommand, AutomatedReview: gate.AutomatedReviewRequired, SecurityAudit: gate.SecurityAuditConfig{Enabled: true}, Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}})
			if err != nil {
				t.Fatal(err)
			}
			cfg := orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked", "In Review", "Merging", "Rework"}, TerminalStates: []string{"Done"}, AutoPromote: orchestrator.AutoPromoteConfig{HumanReview: new(false)}}
			if test.hostHandoff {
				cfg.ActiveStates = append(cfg.ActiveStates, "Rework")
			}
			dependencies := orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: agent, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			orch, err := orchestrator.New(cfg, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			start := func(orch *orchestrator.Orchestrator) func() {
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- orch.Run(ctx) }()
				stop := sync.OnceFunc(func() {
					cancel()
					if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				})
				t.Cleanup(stop)
				return stop
			}
			stopOrchestrator := start(orch)
			deadline, stop := context.WithTimeout(t.Context(), 45*time.Second)
			defer stop()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			deferred := false
			for {
				state, err := orch.State(deadline)
				if err != nil {
					t.Fatalf("observe recovery: deferred=%v source=%d security=%d validator=%d error=%v", deferred, provider.sourceCalls.Load(), provider.securityCalls.Load(), provider.validatorCalls.Load(), err)
				}
				if state.Retry[issue.ID].CompletionDeferred {
					if !deferred && test.publicationField != "" {
						owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
						owner.mu.Lock()
						outcome := owner.preparedOutcome
						owner.mu.Unlock()
						if publicationRefusals.Load() == 0 {
							t.Fatal("audit publication rejection was not exercised")
						}
						if test.publicationField == "run.head_sha" {
							if outcome != "failed" {
								t.Fatalf("source rejection became reviewer continuation: %s", outcome)
							}
							return
						}
						if outcome != "interrupted" {
							t.Fatalf("reviewer rejection failed the source attempt: outcome=%s", outcome)
						}
						detail, err := h.admin.currentChange(deadline, current.WorkItemID)
						if err != nil || len(detail.Versions) != 1 || len(detail.Reviews) != 0 || detail.Summary.Status == "reviewed" {
							t.Fatalf("rejected audit changed published source or became accepted: %+v, %v", detail, err)
						}
						rejectedVersion = detail.Versions[0]
					}
					if !deferred && test.stopped {
						stopOrchestrator()
						owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
						change := owner.NativeChange()
						if err := owner.KeepWorker(func(context.Context) error { return nil }, nil); err != nil {
							t.Fatal(err)
						}
						if err := owner.Finish(t.Context(), "interrupted"); err != nil {
							t.Fatal(err)
						}
						if err := h.native.Release(t.Context(), owner.claim.lease, "cancelled"); err != nil {
							t.Fatal(err)
						}
						var sourceRecovery tracker.NativeSourceRecovery
						if err := h.admin.client.request(t.Context(), "GET", h.admin.base()+"/work-items/"+issue.ID+"/source-recovery", nil, &sourceRecovery); err != nil || sourceRecovery.Quiesced {
							t.Fatalf("interrupted writer became quiescent before cleanup: %+v, %v", sourceRecovery, err)
						}
						if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "cancelled"); err != nil {
							t.Fatal(err)
						}
						if err := h.admin.client.request(t.Context(), "GET", h.admin.base()+"/work-items/"+issue.ID+"/source-recovery", nil, &sourceRecovery); err != nil || !sourceRecovery.Quiesced || sourceRecovery.VersionID != change.VersionID || sourceRecovery.HeadSHA != change.HeadSHA {
							t.Fatalf("confirmed worker cleanup lost recoverable source: %+v, %v", sourceRecovery, err)
						}
						provider.available.Store(true)
						candidate := h.claim(t, issue.ID)
						replacement := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
						result, err := agent.Run(t.Context(), runner.RunRequest{Execution: replacement, Issue: candidate, Mode: runner.RunModeImplement})
						if err != nil || result.NativeChange == nil || !result.NativeChange.Reviewed || result.TurnStarted || provider.writableCalls != 1 {
							t.Fatalf("stopped source did not resume configured review: result=%+v writable=%d error=%v", result, provider.writableCalls, err)
						}
						h.complete(t, issue.ID, result.NativeChange)
						detail, err := h.admin.Change(t.Context(), current.WorkItemID, change.ChangeID)
						if err != nil || len(detail.Versions) != 1 || detail.Versions[0].ID != change.VersionID || detail.Versions[0].HeadSHA != change.HeadSHA || len(detail.Reviews) != 3 || detail.Summary.Status != "reviewed" || provider.securityCalls.Load() != 1 || provider.sourceCalls.Load() != 1 || provider.validatorCalls.Load() != 2 {
							t.Fatalf("recovery changed source or bypassed independent review: detail=%+v error=%v", detail, err)
						}
						return
					}
					if !deferred && test.refusal != "" {
						stopOrchestrator()
						owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
						change := owner.NativeChange()
						if test.refusal == "lease" {
							if err := h.native.Release(t.Context(), owner.claim.lease, "released"); err != nil {
								t.Fatal(err)
							}
						} else {
							if err := os.WriteFile(filepath.Join(source, "replacement.txt"), []byte("replacement source\n"), 0o600); err != nil {
								t.Fatal(err)
							}
							nativeChangeGit(t, source, "add", "replacement.txt")
							nativeChangeGit(t, source, "commit", "-m", "replacement source")
							output, err := exec.CommandContext(t.Context(), "git", "-C", source, "rev-parse", "HEAD").Output()
							if err != nil {
								t.Fatal(err)
							}
							head := strings.TrimSpace(string(output))
							capture, err := workspace.CaptureChangeSource(t.Context(), source, change.BaseSHA, head)
							if err != nil {
								t.Fatal(err)
							}
							h.publishCaptured(t, current.WorkItemID, change.ChangeID, head, &capture, change.VersionID, change.BaseSHA)
						}
						provider.available.Store(true)
						result, err := agent.Run(t.Context(), runner.RunRequest{Execution: owner, DeferExecutionFinish: true, Issue: state.Retry[issue.ID].Issue, Mode: runner.RunModeImplement, NativeReviewContinuation: &runner.NativeReviewContinuation{Source: tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: change.VersionID, HeadSHA: change.HeadSHA}, FinalMessage: owner.preparedMessage}})
						if err == nil || !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) && !errors.Is(err, runner.ErrNativeRecoveryRequired) || provider.writableCalls != 1 || provider.sourceCalls.Load() != 1 || result.TurnStarted {
							t.Fatalf("obsolete owner resumed: result=%+v error=%v", result, err)
						}
						detail, err := h.admin.Change(t.Context(), current.WorkItemID, change.ChangeID)
						if err != nil {
							t.Fatal(err)
						}
						if len(detail.Reviews) != 1 || detail.Reviews[0].SecurityAudit == nil || detail.Summary.Status == "reviewed" {
							t.Fatalf("obsolete owner published evidence: %+v", detail.Reviews)
						}
						return
					}
					if !deferred && test.restore {
						stopOrchestrator()
						machine := h.scheduler.machine
						h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
						if err != nil {
							t.Fatal(err)
						}
						dependencies.Scheduling = h.scheduler
						orch, err = orchestrator.New(cfg, dependencies)
						if err != nil {
							t.Fatal(err)
						}
						stopOrchestrator = start(orch)
					}
					if !deferred && test.name == "source" {
						owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
						change := owner.NativeChange()
						for _, ref := range []tracker.NativeChangeReference{{ChangeID: change.ChangeID, VersionID: "version_other", HeadSHA: change.HeadSHA}, {ChangeID: "change_other", VersionID: change.VersionID, HeadSHA: change.HeadSHA}, {ChangeID: change.ChangeID, VersionID: change.VersionID, HeadSHA: strings.Repeat("a", 40)}} {
							if err := owner.PrepareReview(t.Context(), ref); !errors.Is(err, runner.ErrNativeRecoveryRequired) {
								t.Fatalf("changed source admitted: %+v, %v", ref, err)
							}
						}
						for _, field := range []string{"session", "policy", "project", "machine", "item", "attempt"} {
							var saved nativeCompletionState
							if err := json.Unmarshal(owner.CompletionState(), &saved); err != nil {
								t.Fatal(err)
							}
							switch field {
							case "session":
								saved.Lease.SessionID = "session_other"
							case "policy":
								saved.Lease.PolicyID = "policy_other"
							case "project":
							case "machine":
								saved.Lease.MachineID = "machine_other"
							case "item":
								saved.Lease.WorkItemID = "wi_other"
							case "attempt":
								saved.Data.AttemptID = "attempt_other"
							}
							encoded, err := json.Marshal(saved)
							if err != nil {
								t.Fatal(err)
							}
							request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor}
							if field == "project" {
								request.ProjectID = "other"
							}
							if _, err := h.scheduler.RestoreCompletion(t.Context(), request, state.Retry[issue.ID].Issue, encoded); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
								t.Fatalf("changed %s authority admitted: %v", field, err)
							}
						}
					}
					deferred = true
					provider.available.Store(true)
				}
				current, err := h.admin.Issue(deadline, current.WorkItemID)
				if err != nil {
					t.Fatalf("read recovery: deferred=%v source=%d security=%d validator=%d error=%v", deferred, provider.sourceCalls.Load(), provider.securityCalls.Load(), provider.validatorCalls.Load(), err)
				}
				if test.verdict == "rework" && current.State == "Rework" {
					detail, err := h.admin.currentChange(deadline, current.WorkItemID)
					if err != nil {
						t.Fatal(err)
					}
					finding := false
					for _, review := range detail.Reviews {
						finding = finding || review.Decision == "changes_requested"
					}
					if !deferred || provider.writableCalls != 1 || detail.Summary.Status == "reviewed" || !finding {
						t.Fatalf("genuine finding lost: %+v writable=%d", detail.Summary, provider.writableCalls)
					}
					return
				}
				if test.malformed && deferred && provider.available.Load() && provider.validatorCalls.Load() >= 2 && len(state.Running) == 0 && state.Retry[issue.ID].CompletionDeferred {
					detail, err := h.admin.currentChange(deadline, current.WorkItemID)
					if err != nil {
						t.Fatal(err)
					}
					if detail.Summary.Status == "reviewed" || len(detail.Reviews) != 2 || provider.writableCalls != 1 {
						t.Fatalf("malformed acceptance approved: %+v", detail)
					}
					return
				}
				if current.State == "Merging" {
					detail, err := h.admin.currentChange(deadline, current.WorkItemID)
					if err != nil {
						t.Fatal(err)
					}
					wantVersions := 1
					if test.hostHandoff {
						wantVersions++
					}
					if deferred != (test.stage != "") || len(detail.Versions) != wantVersions || len(detail.Reviews) != 3 || detail.Summary.Status != "reviewed" || provider.writableCalls != 1 {
						t.Fatalf("invalid recovery: deferred=%v versions=%d reviews=%+v writable=%d", deferred, len(detail.Versions), detail.Reviews, provider.writableCalls)
					}
					version := detail.Versions[len(detail.Versions)-1]
					if test.publicationField != "" && (version.ID != rejectedVersion.ID || version.BaseSHA != rejectedVersion.BaseSHA || version.HeadSHA != rejectedVersion.HeadSHA || detail.Reviews[0].SecurityAudit == nil || detail.Reviews[0].SecurityAudit.Run.ReviewerVersion != securityaudit.ReviewerVersion || detail.Reviews[0].SecurityAudit.Run.ReviewerDigest != securityaudit.ReviewerDigest()) {
						t.Fatal("reviewer continuation replaced source or accepted outdated audit evidence")
					}
					if test.hostHandoff {
						if detail.Versions[0].ID != previousVersion.ID || version.HeadSHA == previousVersion.HeadSHA || version.Validation == nil || version.Validation.HeadSHA != version.HeadSHA || version.Validation.TreeSHA == "" || version.Source == nil || version.Source.HeadSHA != version.HeadSHA {
							t.Fatal("handoff lost old source or failed to validate the new source")
						}
					}
					if test.hostHandoff || test.name == "healthy" {
						output, err := os.ReadFile(gateMarker)
						if err != nil || string(output) != "gategategate" {
							t.Fatalf("expected only finalization, source review and validator commands: output=%q error=%v", output, err)
						}
					}
					sessions := map[int64]bool{}
					for _, review := range detail.Reviews {
						if review.VersionID != version.ID {
							t.Fatal("recovery reviewed a different version")
						}
						var session int64
						if review.SecurityAudit != nil {
							session = review.SecurityAudit.SessionID
							if review.SecurityAudit.Run.HeadSHA != version.HeadSHA || review.SecurityAudit.Run.BaseSHA != version.BaseSHA {
								t.Fatal("audit lost source binding")
							}
						}
						for _, result := range []*gate.ValidatorResult{review.ImplementationReview, review.Validator} {
							if result != nil {
								session = result.SessionID
								if result.HeadSHA != version.HeadSHA || result.BaseSHA != version.BaseSHA || result.Repository != version.Repository {
									t.Fatal("review lost source binding")
								}
							}
						}
						if session <= 0 || sessions[session] {
							t.Fatal("review stages reused a session")
						}
						sessions[session] = true
					}
					calls := map[string]int32{"source": provider.sourceCalls.Load(), "security": provider.securityCalls.Load(), "validator": provider.validatorCalls.Load()}
					for stage, count := range calls {
						want := int32(1)
						if stage == test.stage {
							want++
						}
						if count != want {
							t.Fatalf("%s calls=%d, want %d", stage, count, want)
						}
					}
					return
				}
				select {
				case <-deadline.Done():
					t.Fatalf("review did not recover: deferred=%v source=%d security=%d validator=%d state=%s", deferred, provider.sourceCalls.Load(), provider.securityCalls.Load(), provider.validatorCalls.Load(), current.State)
				case <-ticker.C:
				}
			}
		})
	}
}
