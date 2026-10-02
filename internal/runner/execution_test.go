package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type testExecution struct {
	recovery    tracker.NativeRecovery
	validateErr error
	checkpoint  *tracker.NativeCheckpoint
	finish      string
	started     bool
}

func (e *testExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, e.validateErr
}

func (e *testExecution) Validate(context.Context) error { return e.validateErr }
func (e *testExecution) Start(context.Context, tracker.NativeExecutionIdentity) error {
	e.started = true
	return nil
}
func (e *testExecution) Checkpoint(_ context.Context, checkpoint tracker.NativeCheckpoint) error {
	e.checkpoint = &checkpoint
	return nil
}
func (e *testExecution) Finish(_ context.Context, outcome string) error {
	e.finish = outcome
	return nil
}
func (e *testExecution) Recovery() tracker.NativeRecovery { return e.recovery }

type retainedExecutionWorkspace struct {
	*fakeWorkspaceBackend
	retained bool
}

func (w *retainedExecutionWorkspace) PreserveIssue(context.Context, workspace.Issue) (workspace.Preservation, error) {
	w.retained = true
	return workspace.Preservation{Preserved: true}, nil
}

func TestNativeRecoveryDecision(t *testing.T) {
	t.Parallel()
	identity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
	for _, test := range []struct {
		name   string
		edit   func(*tracker.NativeRecovery, **workspace.RecoveryState, *bool)
		action string
		reason string
	}{
		{"verified session", func(*tracker.NativeRecovery, **workspace.RecoveryState, *bool) {}, "resume_session", "verified_local_session"},
		{"first run", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Attempts = nil }, "fresh_checkout", "no_prior_attempt"},
		{"missing checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Attempts[0].Checkpoint = nil }, "fresh_checkout", "checkpoint_missing"},
		{"machine lost with dirty work", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Lease.MachineID = "other" }, "manual_recovery", "checkpoint_unavailable"},
		{"local workspace missing", func(_ *tracker.NativeRecovery, local **workspace.RecoveryState, _ *bool) { *local = nil }, "manual_recovery", "checkpoint_unavailable"},
		{"dirty checkpoint replaced", func(_ *tracker.NativeRecovery, local **workspace.RecoveryState, _ *bool) {
			(*local).WorkspaceFingerprint = "different"
		}, "manual_recovery", "local_checkpoint_changed"},
		{"inaccessible checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Availability = "inaccessible"
		}, "manual_recovery", "checkpoint_unavailable"},
		{"customer receipt unverified", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Storage = "customer_store"
		}, "manual_recovery", "checkpoint_unavailable"},
		{"clean inaccessible checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Availability = "missing"
			r.Attempts[0].Checkpoint.WorktreeState = "clean"
		}, "fresh_checkout", "checkpoint_unavailable"},
		{"provider session missing", func(_ *tracker.NativeRecovery, _ **workspace.RecoveryState, available *bool) { *available = false }, "fresh_checkout", "session_restart_required"},
		{"policy changed", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Lease.PolicyID = "new-policy" }, "fresh_checkout", "session_restart_required"},
		{"backend changed", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "claude", Model: "test"}
		}, "fresh_checkout", "session_restart_required"},
		{"push ambiguity", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.ExternalEffect = "git_push"
			r.Attempts[0].Checkpoint.EffectState = "ambiguous"
		}, "manual_recovery", "external_effect_uncertain"},
		{"PR pending", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.ExternalEffect = "pr_create"
			r.Attempts[0].Checkpoint.EffectState = "pending"
		}, "manual_recovery", "external_effect_uncertain"},
		{"manual checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Resume = "manual_recovery"
		}, "manual_recovery", "checkpoint_requires_recovery"},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := &workspace.RecoveryState{HeadSHA: "head", WorkspaceFingerprint: "digest"}
			available := true
			recovery := tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "machine", PolicyID: "policy"}, Attempts: []tracker.NativeAttempt{{
				NativeRunData: tracker.NativeRunData{Identity: &identity, MachineID: "machine", PolicyID: "policy"},
				Checkpoint:    &tracker.NativeCheckpoint{Resume: "resume_session", Availability: "available", Storage: "local_only", WorktreeState: "dirty", HeadSHA: "head", WorkspaceDigest: "digest", ExternalEffect: "none", EffectState: "none"},
			}}}
			test.edit(&recovery, &local, &available)
			action, reason := nativeRecoveryAction(recovery, local, available, identity)
			if action != test.action || reason != test.reason {
				t.Fatalf("recovery = %s/%s, want %s/%s", action, reason, test.action, test.reason)
			}
		})
	}
}

func TestNativeEpiloguePreservesBeforeCleanup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		state    workspace.RecoveryState
		lost     bool
		retained bool
		after    bool
	}{
		{"clean", workspace.RecoveryState{HeadSHA: "head"}, false, false, true},
		{"dirty", workspace.RecoveryState{TrackedPaths: []string{"work.go"}}, false, true, false},
		{"unpushed", workspace.RecoveryState{UnpushedCommits: 2}, false, true, false},
		{"claim lost", workspace.RecoveryState{TrackedPaths: []string{"work.go"}}, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{test.state}}}
			execution := &testExecution{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.lost {
				execution.validateErr = ErrExecutionAuthorityUnavailable
				cancel()
			}
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
			err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{}, workspace.Issue{})
			if errors.Is(err, ErrExecutionAuthorityUnavailable) != test.lost || backend.retained != test.retained || backend.afterRun != test.after {
				t.Fatalf("epilogue error=%v retained=%v hook=%v", err, backend.retained, backend.afterRun)
			}
			if test.lost && execution.checkpoint != nil {
				t.Fatal("lost owner wrote a checkpoint")
			}
			if !test.lost && execution.checkpoint == nil {
				t.Fatal("epilogue omitted checkpoint")
			}
		})
	}
}

func TestNativeGuardPreventsRun(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	_, err := r.Run(t.Context(), RunRequest{Execution: &testExecution{validateErr: ErrExecutionAuthorityUnavailable}})
	if !errors.Is(err, ErrExecutionAuthorityUnavailable) {
		t.Fatalf("guard = %v", err)
	}
}

type executionUnavailableBackend struct{}

func (executionUnavailableBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, ErrExecutionAuthorityUnavailable
}

func TestNativeOutagePreservesFailureBudget(t *testing.T) {
	t.Parallel()
	supervisor, err := NewSupervisor(executionUnavailableBackend{}, SupervisorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	completion := supervisor.Run(t.Context(), RunRequest{Attempt: 4})
	if !completion.Retryable || completion.RetryAttempt != 4 || completion.RetryDelay != supervisor.OverloadRetryDelay() {
		t.Fatalf("outage consumed retry budget: %#v", completion)
	}
}

func TestNativeRecoveryPromptIncludesContext(t *testing.T) {
	t.Parallel()
	execution := &testExecution{recovery: tracker.NativeRecovery{Issue: tracker.NativeIssue{Title: "Native issue"}, Discussion: []tracker.NativeComment{{Body: "Prior discussion"}}, Attempts: []tracker.NativeAttempt{{Status: "interrupted"}}}}
	prompt, err := nativeRecoveryPrompt(execution)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"Native issue", "Prior discussion", "interrupted", "untrusted task content", "Do not fetch GitHub issue history"} {
		if !strings.Contains(prompt, content) {
			t.Errorf("prompt omitted %q", content)
		}
	}
}

type nativeExecutionTransport func(*http.Request) (*http.Response, error)

func (f nativeExecutionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNativeRunnerPublishesOnlyAfterRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		blocked    bool
		workerAuth bool
	}{
		{name: "first run without worker GitHub access"},
		{name: "lost checkpoint", blocked: true},
		{name: "explicit worker GitHub access", workerAuth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}}}
			agent := &fakeCodexClient{}
			execution := &testExecution{}
			if test.blocked {
				execution.recovery = tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "new-machine"}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{MachineID: "lost-machine"}, Checkpoint: &tracker.NativeCheckpoint{Storage: "local_only", WorktreeState: "dirty", Resume: "resume_session"}}}}
			}
			cfg := config.Config{}
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Tracker.APIKey = "$NATIVE_HUB_TOKEN"
			cfg = cfg.WithRuntimeGitHubToken("native-instance-token")
			if test.workerAuth {
				cfg.Worker.GitHubToken = "$NATIVE_WORKER_GITHUB_TOKEN"
				cfg.Worker.GitHubRESTMinReserve = 500
				cfg.Worker.GitHubRESTPollIntervalMS = 3600000
			}
			cliDir := t.TempDir()
			cliName := "gh"
			cliBody := "#!/bin/sh\n[ -r \"$GH_CONFIG_DIR/hosts.yml\" ] || exit 1\nprintf %s native-worker-token\n"
			if runtime.GOOS == "windows" {
				cliName = "gh.bat"
				cliBody = "@echo off\r\nif not exist \"%GH_CONFIG_DIR%\\hosts.yml\" exit /b 1\r\necho native-worker-token\r\n"
			}
			if err := os.WriteFile(filepath.Join(cliDir, cliName), []byte(cliBody), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			identityReads, budgetReads := 0, 0
			originalClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
				if !test.workerAuth || req.URL.Host != "api.github.com" || req.Header.Get("Authorization") != "Bearer native-worker-token" {
					t.Errorf("unexpected worker GitHub request: %s", req.URL)
					return nil, errors.New("unexpected worker GitHub request")
				}
				switch req.URL.Path {
				case "/graphql":
					identityReads++
					return workerGitHubPrincipalResponse(), nil
				case "/rate_limit":
					budgetReads++
					return workerGitHubRateLimitResponse(), nil
				default:
					t.Errorf("unexpected worker GitHub path: %s", req.URL.Path)
					return nil, errors.New("unexpected worker GitHub path")
				}
			})}
			t.Cleanup(func() { http.DefaultClient = originalClient })
			githubCredentialReads := 0
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent, lookupEnv: func(key string) string {
				githubCredentialReads++
				if key == "NATIVE_WORKER_GITHUB_TOKEN" {
					return "native-worker-token"
				}
				t.Errorf("resolved an unused credential: %s", key)
				return ""
			}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
			if test.workerAuth {
				if githubCredentialReads != 1 || identityReads != 1 || budgetReads != 1 {
					t.Fatalf("worker credential/identity/budget reads = %d/%d/%d, want 1/1/1", githubCredentialReads, identityReads, budgetReads)
				}
				policy := agent.request.workerGitHub
				if !policy.Enabled || policy.Token != "native-worker-token" || policy.PrincipalID != 42 || policy.Principal.Login != "detent-worker[bot]" {
					t.Fatal("native worker lost its selected credential or principal")
				}
				variables := agent.request.Environment.Variables
				if variables["GH_CONFIG_DIR"] != filepath.Join(agent.request.TempDir, "github-cli") || variables["GH_TOKEN"] != "" || variables["GITHUB_TOKEN"] != "" {
					t.Fatal("native worker lost its isolated GitHub environment")
				}
				if result.RateLimits == nil || len(result.RateLimits.GitHubRESTBudgets) != 1 {
					t.Fatal("native worker omitted its GitHub budget accounting")
				}
				budget := result.RateLimits.GitHubRESTBudgets[0]
				if budget.CredentialIdentity != policy.CredentialIdentity || budget.CredentialIdentity == "" || budget.Consumer != telemetry.RESTConsumerWorker || budget.Remaining != 4200 || budget.MinRemainingReserve != 500 {
					t.Fatalf("native worker budget = %+v", budget)
				}
			} else if githubCredentialReads != 0 || identityReads != 0 || budgetReads != 0 {
				t.Fatal("native coding resolved or used an unused GitHub credential")
			}
			if errors.Is(err, ErrNativeRecoveryRequired) != test.blocked || (!test.blocked && err != nil) {
				t.Fatalf("run error = %v", err)
			}
			if test.blocked && execution.started {
				t.Fatal("unresolved checkpoint was superseded by a new attempt")
			}
			if !test.blocked && (!execution.started || execution.finish == "" || execution.checkpoint == nil) {
				t.Fatalf("native run omitted lifecycle: %#v", execution)
			}
		})
	}
}

type artifactExecutionProbe struct {
	testExecution
	failure   error
	finalized bool
	evidence  []ValidationEvidence
}

func (*artifactExecutionProbe) PrepareArtifacts(context.Context, string) error { return nil }
func (*artifactExecutionProbe) ArtifactLog(context.Context, string) error      { return nil }
func (e *artifactExecutionProbe) FinalizeArtifacts(context.Context, string) error {
	e.finalized = true
	return e.failure
}

func (e *artifactExecutionProbe) PublishValidationEvidence(_ context.Context, files []ValidationEvidence) error {
	e.evidence = files
	return nil
}

func TestArtifactsFinalizeBeforeWorkspaceCleanup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		state       workspace.RecoveryState
		recoveryErr error
		failed      bool
		finalized   bool
		after       bool
		screenshot  bool
	}{
		{name: "clean with screenshots", screenshot: true, state: workspace.RecoveryState{HeadSHA: "head"}, finalized: true, after: true},
		{name: "clean", state: workspace.RecoveryState{HeadSHA: "head"}, finalized: true, after: true},
		{name: "failed capture", state: workspace.RecoveryState{HeadSHA: "head"}, failed: true, finalized: true},
		{name: "unpushed finalized head", state: workspace.RecoveryState{HeadSHA: "head", UnpushedCommits: 1}, finalized: true},
		{name: "dirty source cannot freeze artifacts", state: workspace.RecoveryState{HeadSHA: "head", TrackedPaths: []string{"source.go"}}},
		{name: "unavailable recovery cannot freeze artifacts", recoveryErr: errors.New("recovery unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{test.state}, recoveryErr: test.recoveryErr}}
			execution := &artifactExecutionProbe{}
			directory := t.TempDir()
			if test.screenshot {
				path := filepath.Join(directory, ".detent", "validation", "1")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "test.png"), []byte("screenshot bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test.failed {
				execution.failure = errors.New("upload unavailable")
			}
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
			err := r.afterExecution(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{Path: directory}, workspace.Issue{})
			if test.screenshot && (len(execution.evidence) != 1 || string(execution.evidence[0].Content) != "screenshot bytes" || execution.evidence[0].Name != "test.png") {
				t.Fatalf("evidence=%+v", execution.evidence)
			}
			if execution.finalized != test.finalized || backend.afterRun != test.after || (err != nil) != test.failed {
				t.Fatal("cleanup preceded durable finalization", err, backend.afterRun)
			}
		})
	}
}

type wipExecutionWorkspace struct {
	retainedExecutionWorkspace
	published      bool
	publishErr     error
	publishedState *workspace.RecoveryState
}

func (w *wipExecutionWorkspace) PublishWorkInProgress(ctx context.Context, _ workspace.Issue, validate func(context.Context) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validate(ctx); err != nil {
		return err
	}
	w.published = true
	if w.publishErr == nil && w.publishedState != nil {
		w.recoveryStates = []workspace.RecoveryState{*w.publishedState}
	}
	return w.publishErr
}

func TestAvailabilityDeadlinePublishesBeforeFinish(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(strconv.FormatBool(failed), func(t *testing.T) {
			backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}, publishedState: &workspace.RecoveryState{HeadSHA: "published-head", WorkspaceFingerprint: "published-digest"}}
			if failed {
				backend.publishErr = errors.New("push unavailable")
			}
			execution := &availabilityTestExecution{deadline: time.Now().Add(-time.Second)}
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(context.Canceled)
			defer cancel(context.Canceled)
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
			err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{}, workspace.Issue{})
			if !backend.published || backend.afterRun {
				t.Fatalf("published=%t cleaned=%t", backend.published, backend.afterRun)
			}
			if failed && !errors.Is(err, backend.publishErr) {
				t.Fatalf("publish error lost: %v", err)
			}
			if execution.checkpoint == nil || !failed && (execution.checkpoint.WorktreeState != "clean" || execution.checkpoint.HeadSHA != "published-head" || execution.checkpoint.WorkspaceDigest != "published-digest") {
				t.Fatalf("final checkpoint = %#v", execution.checkpoint)
			}
		})
	}
}

type availabilityTestExecution struct {
	testExecution
	deadline time.Time
}

func (e *availabilityTestExecution) AvailabilityDeadline() time.Time { return e.deadline }

func (e *availabilityTestExecution) Validate(ctx context.Context) error { return ctx.Err() }

func (e *availabilityTestExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.testExecution.Checkpoint(ctx, checkpoint)
}

type deadlineRunExecution struct {
	availabilityTestExecution
	cancel    context.CancelCauseFunc
	published *bool
}

func (e *deadlineRunExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	guarded, cancel := context.WithCancelCause(ctx)
	e.cancel = cancel
	return guarded, func() { cancel(context.Canceled) }, nil
}

func (e *deadlineRunExecution) Validate(ctx context.Context) error { return ctx.Err() }

func (e *deadlineRunExecution) Finish(ctx context.Context, outcome string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !*e.published {
		return errors.New("finish preceded WIP publication")
	}
	e.finish = outcome
	return nil
}

type availabilityStoppingBackend struct {
	fakeCodexClient
	stop func()
	err  error
}

func (b *availabilityStoppingBackend) RunTurn(ctx context.Context, _ AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.stop()
	<-ctx.Done()
	return AgentTurnResult{}, errors.Join(ctx.Err(), b.err)
}

func TestRunnerAvailabilityInterruptionFinishesAfterWIP(t *testing.T) {
	t.Parallel()
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}, recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}}
	execution := &deadlineRunExecution{availabilityTestExecution: availabilityTestExecution{deadline: time.Now().Add(-time.Second)}, published: &backend.published}
	agent := &availabilityStoppingBackend{stop: func() { execution.cancel(context.Canceled) }}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	held, released := false, false
	r.sleepInhibitor = func(context.Context, func()) (func(), error) { held = true; return func() { released = true }, nil }
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if execution.finish != "interrupted" || !backend.published || !backend.retained || backend.afterRun {
		t.Fatalf("finish=%s published=%t retained=%t cleaned=%t", execution.finish, backend.published, backend.retained, backend.afterRun)
	}
	if !held || !released {
		t.Fatalf("sleep held=%t released=%t", held, released)
	}
}

type availabilityCancelledBackend struct{}

func (availabilityCancelledBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, context.Canceled
}

func TestAvailabilityStopPreservesRetryBudget(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(strconv.FormatBool(expired), func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			if expired {
				deadline = time.Now().Add(-time.Second)
			}
			execution := &availabilityTestExecution{deadline: deadline}
			supervisor, err := NewSupervisor(availabilityCancelledBackend{}, SupervisorConfig{})
			if err != nil {
				t.Fatal(err)
			}
			completion := supervisor.Run(t.Context(), RunRequest{Attempt: 4, Execution: execution})
			want := 5
			if expired {
				want = 4
			}
			if !completion.Retryable || completion.RetryAttempt != want {
				t.Fatalf("retry = %t/%d, want attempt %d", completion.Retryable, completion.RetryAttempt, want)
			}
		})
	}
}

func TestAvailabilityStopRetainsUnreapedWorkspace(t *testing.T) {
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}, recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}}
	execution := &deadlineRunExecution{availabilityTestExecution: availabilityTestExecution{deadline: time.Now().Add(-time.Second)}, published: &backend.retained}
	agent := &availabilityStoppingBackend{stop: func() { execution.cancel(context.Canceled) }, err: ErrWorkerProcessReap}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if !errors.Is(err, ErrWorkerProcessReap) || backend.published || !backend.retained || backend.afterRun {
		t.Fatalf("error=%v published=%t retained=%t cleaned=%t", err, backend.published, backend.retained, backend.afterRun)
	}
}

func TestAvailabilityStopFinalizesLocalSessionAfterPushFailure(t *testing.T) {
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}, recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}, publishErr: errors.New("push unavailable")}
	execution := &deadlineRunExecution{availabilityTestExecution: availabilityTestExecution{deadline: time.Now().Add(-time.Second)}, published: &backend.published}
	agent := &availabilityStoppingBackend{stop: func() { execution.cancel(context.Canceled) }}
	sessionStore := &fakeSessionStore{sessionID: 3169}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent, Store: sessionStore})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if !errors.Is(err, backend.publishErr) || sessionStore.finishCalls != 1 || sessionStore.usageCalls != 1 || execution.finish != "interrupted" {
		t.Fatalf("error=%v session finishes=%d usage=%d outcome=%s", err, sessionStore.finishCalls, sessionStore.usageCalls, execution.finish)
	}
}
