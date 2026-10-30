package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/compute"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestSSHErrorRoundTrip(t *testing.T) {
	t.Parallel()
	acceptanceFailure := func(err error) bool {
		failure := nativeTerminalFailure(err, time.Now().UTC(), false)
		return failure != nil && failure.Rejection != nil && failure.Rejection.Category == "acceptance_validation" && failure.Rejection.Field == "validator.session_id" && failure.Stage == "acceptance_publication" && failure.ErrorClass == "acceptance_publication" && failure.Attribution == "" && failure.Error == "publication rejected: category=acceptance_validation field=validator.session_id [private error text omitted]"
	}
	tests := []struct {
		name     string
		err      error
		sentinel error
		check    func(error) bool
	}{
		{"workspace", fmt.Errorf("setup: %w", ErrWorkspacePreparation), ErrWorkspacePreparation, func(err error) bool {
			return err.Error() == "setup: "+ErrWorkspacePreparation.Error()
		}},
		{"not found", store.ErrNotFound, store.ErrNotFound, nil},
		{"superseded Change source", ErrChangeSourceVersionSuperseded, ErrChangeSourceVersionSuperseded, nil},
		{"superseded Change source identities", fmt.Errorf("%w (Change %q, selected version %q, current version %q)", ErrChangeSourceVersionSuperseded, "change_example", "version_selected", "version_current"), ErrChangeSourceVersionSuperseded, func(err error) bool {
			return strings.Contains(err.Error(), `Change "change_example", selected version "version_selected", current version "version_current"`)
		}},
		{"acceptance rejection", acceptanceRejectionError{}, nil, acceptanceFailure},
		{"wrapped acceptance rejection", fmt.Errorf("%w: acceptance publication: %w", ErrValidatorInfrastructure, acceptanceRejectionError{}), ErrValidatorInfrastructure, acceptanceFailure},
		{"cancelled", context.Canceled, context.Canceled, nil},
		{"artifact quota", artifact.ErrQuota, artifact.ErrQuota, nil},
		{"artifact storage", artifact.ErrStorage, artifact.ErrStorage, nil},
		{"GitHub quota authority", &github.StatusError{StatusCode: 429, Err: github.ErrRateLimited, CredentialIdentity: "local-credential", RateLimitKind: "secondary_throttled", RetryAfter: 120 * time.Second, ObservedAt: time.Date(2026, 10, 1, 21, 42, 42, 0, time.UTC)}, github.ErrRateLimited, func(err error) bool {
			var status *github.StatusError
			return errors.As(err, &status) && status.CredentialIdentity == "local-credential" && status.RetryAfter == 120*time.Second && !status.ObservedAt.IsZero()
		}},
		{"landing refusal", ErrLandingNotReviewed, ErrLandingNotReviewed, nil},
		{"landing conflict", &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "conflict"}, nil, func(err error) bool {
			var refusal *workspace.LandRefusal
			return errors.As(err, &refusal) && refusal.Kind == workspace.LandRefusalConflict
		}},
		{"landing validation", &workspace.ValidationError{Output: "failed package", Err: context.DeadlineExceeded}, context.DeadlineExceeded, func(err error) bool {
			var validation *workspace.ValidationError
			return errors.As(err, &validation) && validation.Output == "failed package"
		}},
		{"missing hook shell", &workspace.HookError{Hook: "after_create", ExitCode: -1, Err: &shell.UnavailableError{Shell: "bash"}}, workspace.ErrHookFailed, func(err error) bool {
			var unavailable *shell.UnavailableError
			return errors.As(err, &unavailable) && unavailable.Shell == "bash"
		}},
		{"joined", errors.Join(ErrWorkspacePreparation, context.DeadlineExceeded), context.DeadlineExceeded, nil},
		{"capacity", backendcapacity.NewError(backendcapacity.Scope{BackendID: "code"}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload}, errors.New("busy")), nil, func(err error) bool {
			e, ok := backendcapacity.As(err)
			return ok && e.Scope.BackendID == "code" && IsTransientOverload(err)
		}},
		{"credential", &WorkerGitHubBudgetMonitorError{CredentialIdentity: "isolated", Operation: "probe", Err: context.DeadlineExceeded}, context.DeadlineExceeded, func(err error) bool {
			e, ok := AsWorkerGitHubBudgetMonitorError(err)
			return ok && e.CredentialIdentity == "isolated" && e.Operation == "probe"
		}},
		{"configuration", &IssueConfigurationError{Field: "effort", Reason: "invalid"}, nil, func(err error) bool { var e *IssueConfigurationError; return errors.As(err, &e) && e.Field == "effort" }},
		{"source review parse failure", &NativeImplementationReviewError{Err: errors.New("malformed source review")}, nil, func(err error) bool {
			var status interface{ BackendErrorStatus() string }
			return errors.As(err, &status) && status.BackendErrorStatus() == "interrupted" && errors.Unwrap(err) != nil && err.Error() == "malformed source review"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(encodeSSHError(test.err))
			if err != nil {
				t.Fatal(err)
			}
			var wire sshError
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			got := wire.err()
			if test.sentinel != nil && !errors.Is(got, test.sentinel) {
				t.Fatalf("lost sentinel: %v", got)
			}
			if test.check != nil && !test.check(got) {
				t.Fatalf("lost structured error: %T %v", got, got)
			}
		})
	}
}

func TestSSHPeerConcurrentCallbacksAndDisconnect(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	execution := &readToolTestExecution{}
	callback := NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil)
	publicationOwner := &artifactExecutionProbe{testExecution: testExecution{validateErr: errors.New("public validation is forbidden during PrepareFinish"), onCheckpoint: func(tracker.NativeCheckpoint) {
		t.Error("publication transport called public Checkpoint during PrepareFinish")
	}}}
	callbacks := (&Runner{}).SSHRunCallbacks(RunRequest{Execution: publicationOwner})
	callbacks.handle = func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		if method == "execution.PrepareFinish" || method == "execution.RecordPipelineTiming" {
			return callback(ctx, method, args)
		}
		if method != "echo" {
			return nil, errors.New("unexpected callback")
		}
		var value int
		err := json.Unmarshal(args[0], &value)
		return value, err
	}
	central := NewSSHPeer(t.Context(), a, a, callbacks.Handle)
	callbacks.BindExecutionSources(central, t.TempDir())
	t.Cleanup(central.Close)
	var remote *SSHPeer
	sources := &SSHExecutionSources{}
	var invocation atomic.Uint64
	ready := make(chan struct{})
	remote = NewSSHPeer(t.Context(), b, b, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		<-ready
		if method == "source.publication" {
			var id uint64
			if len(args) > 0 {
				if err := json.Unmarshal(args[0], &id); err != nil {
					return nil, err
				}
			}
			if id != 0 {
				invocation.Store(id)
			}
			return sources.Handle(ctx, method, args)
		}
		var value int
		if err := remote.Call(ctx, "echo", &value, 42); err != nil {
			return nil, err
		}
		return value, nil
	})
	close(ready)
	t.Cleanup(remote.Close)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			var value int
			if err := central.Call(t.Context(), "nested", &value); err != nil || value != 42 {
				t.Errorf("nested callback = %d, %v", value, err)
			}
		})
	}
	wg.Wait()
	code, maxChars, actualChars := -32602, int64(1048576), int64(2927066)
	failure := tracker.NativeTerminalFailure{ObservedAt: time.Now().UTC(), Provider: "codex", Operation: "turn/start", RPCCode: &code, ProviderCode: "input_too_large", MaxChars: &maxChars, ActualChars: &actualChars}
	if err := (&sshNativeExecution{sshExecution: &sshExecution{peer: remote}}).PrepareFinish(t.Context(), "failed", "failed request", &failure); err != nil {
		t.Fatal(err)
	}
	got := execution.completionFailure
	if execution.completionBody != "failed request" || got == nil || got.Operation != failure.Operation || got.RPCCode == nil || *got.RPCCode != code || got.MaxChars == nil || *got.MaxChars != maxChars || got.ActualChars == nil || *got.ActualChars != actualChars || !got.ObservedAt.Equal(failure.ObservedAt) {
		t.Fatalf("SSH completion dropped provider failure: %+v", got)
	}
	remoteExecution := &sshNativeExecution{sshExecution: &sshExecution{peer: remote}, sources: sources}
	for _, stage := range []string{"worker_check_land", "rebase", "conflict_resolution"} {
		t.Run("pipeline/"+stage, func(t *testing.T) {
			start := time.Now().UTC()
			timing := gate.Interval(stage, start, start.Add(time.Second), "clean")
			recorder, ok := any(remoteExecution).(PipelineExecution)
			if !ok {
				t.Fatal("SSH execution does not forward pipeline evidence")
			}
			before := len(execution.pipeline)
			recorder.RecordPipelineTiming(timing)
			if len(execution.pipeline) != before+1 || execution.pipeline[before] != timing {
				t.Fatalf("SSH pipeline evidence: %+v", execution.pipeline)
			}
		})
	}
	version := tracker.ChangeVersion{ID: "version", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example/repo", HeadSHA: strings.Repeat("a", 40), PolicyID: "policy"}}
	identity := workspace.GitHubPublication{Repository: version.Repository, HeadSHA: version.HeadSHA, Branch: "detent/rework", BaseRef: "develop"}
	pushed := workspace.GitHubPublicationPush{EffectID: "effect_retained", Publication: tracker.NativePRPublication{ChangeID: "change", VersionID: "previous", Repository: version.Repository, HeadSHA: strings.Repeat("b", 40), PolicyID: "previous-policy", SourceVersion: tracker.NativeChangeReference{ChangeID: "change", VersionID: "previous", HeadSHA: strings.Repeat("b", 40)}, SourceAttemptID: "attempt_previous", External: tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}}}
	remoteExecution.setPublicationIdentity(func(_ context.Context, _ tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
		if opts.External != nil && (len(opts.PublicationPushes) != 1 || opts.PublicationPushes[0] != pushed) {
			return workspace.GitHubPublication{}, errors.New("publication identity forwarding lost historical push authority")
		}
		return identity, nil
	})
	var privateOwner sync.Mutex
	for _, mode := range []string{"confirmed", "nested callback", "retained branch", "wrong head", "wrong base", "wrong repository", "wrong branch", "wrong version", "stale invocation", "merge", "cancelled", "lost authority", "ambiguous", "disconnect"} {
		t.Run("publication/"+mode, func(t *testing.T) {
			identity.Branch = "detent/rework"
			if mode == "retained branch" {
				identity.Branch = "manual/recovery"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var authorizations, effects atomic.Int32
			refusal := errors.New("current lease was lost")
			opts := workspace.LandOptions{Authorize: func(ctx context.Context) error {
				authorizations.Add(1)
				if err := ctx.Err(); err != nil {
					return err
				}
				if mode == "lost authority" && authorizations.Load() >= 4 {
					return refusal
				}
				return nil
			}, PublicationEffect: func(_ context.Context, kind, state string, value workspace.GitHubPublication) error {
				effects.Add(1)
				if mode == "nested callback" {
					var result int
					if err := central.Call(ctx, "nested", &result); err != nil {
						return err
					}
					if result != 42 {
						return errors.New("publication checkpoint lost its nested worker callback")
					}
				}
				if kind == "pr_create" && state == "confirmed" && value.External.ID != "7" {
					t.Error("PR identity was dropped")
				}
				return nil
			}}
			if mode == "retained branch" {
				opts.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}
				opts.ExternalHeadSHA = strings.Repeat("b", 40)
				opts.PublicationPushes = []workspace.GitHubPublicationPush{pushed}
			}
			remoteExecution.SetPublicationSource(func(ctx context.Context, received tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
				if received.ID != version.ID || opts.Repository != version.Repository || opts.HeadSHA != version.HeadSHA || opts.TargetBranch != "develop" || opts.Authorize == nil || opts.PublicationEffect == nil || opts.GitHubClient != nil {
					return workspace.GitHubPublication{}, errors.New("publication forwarding lost exact binding or leaked a client")
				}
				if mode == "retained branch" && (opts.External == nil || opts.External.ID != "7" || opts.ExternalHeadSHA != strings.Repeat("b", 40) || len(opts.PublicationPushes) != 1 || opts.PublicationPushes[0] != pushed) {
					return workspace.GitHubPublication{}, errors.New("publication forwarding lost the retained PR identity")
				}
				if mode == "cancelled" {
					cancel()
				}
				if err := opts.Authorize(ctx); err != nil {
					return workspace.GitHubPublication{}, err
				}
				if mode == "disconnect" {
					b.Close()
				}
				if mode == "merge" {
					return workspace.GitHubPublication{}, remote.Call(ctx, "publication.merge", nil)
				}
				if strings.HasPrefix(mode, "wrong") || mode == "stale invocation" {
					target := identity
					id, versionID := invocation.Load(), version.ID
					switch mode {
					case "wrong head":
						target.HeadSHA = strings.Repeat("b", 40)
					case "wrong repository":
						target.Repository += "-other"
					case "wrong branch":
						target.Branch += "-other"
					case "wrong base":
						target.BaseRef = "main"
					case "wrong version":
						versionID = "replaced"
					case "stale invocation":
						id++
					}
					return workspace.GitHubPublication{}, remote.Call(ctx, "publication.effect", nil, id, versionID, target, "git_push", "pending", target)
				}
				if err := opts.PublicationEffect(ctx, "git_push", "confirmed", identity); err != nil {
					return workspace.GitHubPublication{}, err
				}
				if mode == "ambiguous" {
					if err := opts.PublicationEffect(ctx, "pr_create", "ambiguous", identity); err != nil {
						return workspace.GitHubPublication{}, err
					}
					return workspace.GitHubPublication{}, errors.New("create response lost")
				}
				result := identity
				result.External = tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}
				if err := opts.PublicationEffect(ctx, "pr_create", "confirmed", result); err != nil {
					return workspace.GitHubPublication{}, err
				}
				return result, nil
			})
			privateOwner.Lock()
			result, err := publicationOwner.publicationSource(ctx, version, opts)
			privateOwner.Unlock()
			if mode == "confirmed" || mode == "nested callback" || mode == "retained branch" {
				if err != nil || result.External.ID != "7" || effects.Load() != 2 {
					t.Fatalf("SSH held publication=%s effects=%d error=%v", result.External.ID, effects.Load(), err)
				}
			} else if err == nil || result.External.ID != "" || mode != "ambiguous" && effects.Load() != 0 {
				t.Fatalf("unbound or uncertain SSH publication succeeded: mode=%s effects=%d error=%v", mode, effects.Load(), err)
			}
			if err := remote.Call(t.Context(), "publication.authorize", nil, invocation.Load(), version.ID, identity); err == nil {
				t.Fatal("publication callback survived its invocation")
			}
		})
	}
	b.Close()
	<-central.Context().Done()
	if err := central.Call(t.Context(), "closed", nil); err == nil {
		t.Fatal("disconnected peer accepted a call")
	}
}

func TestSSHCallbackDoesNotPublishRemotePID(t *testing.T) {
	t.Parallel()
	var got UsageUpdate
	request := RunRequest{OnUsageUpdate: func(update UsageUpdate) error { got = update; return nil }}
	handler := NewSSHCallbackHandler(request, nil, nil, nil)
	data, err := json.Marshal(UsageUpdate{WorkerProcess: procgroup.Identity{PID: 12345}, DetentSessionID: 77, WorkspacePath: "/remote/work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler(t.Context(), "usage", []json.RawMessage{data}); err != nil {
		t.Fatal(err)
	}
	if got.WorkerProcess != (procgroup.Identity{}) || got.DetentSessionID != 77 || got.WorkspacePath != "/remote/work" {
		t.Fatalf("unsafe or lost usage: %+v", got)
	}
	for _, method := range []string{"store.UpdateSessionWorkerProcess", "store.MarkSessionWorkerProcessReaped", "store.Close"} {
		if _, err := handler(t.Context(), method, nil); err == nil {
			t.Fatalf("allowed %s", method)
		}
	}
	if _, ok := any(SSHSessionStore{}).(sessionWorkerProcessStore); ok {
		t.Fatal("remote PIDs can enter the local reaper")
	}
	if _, ok := any(SSHSessionStore{}).(sessionWorkerProcessReaper); ok {
		t.Fatal("remote store exposes the local process registry")
	}
}

func TestSSHRunResponseRetainsResultOnFailure(t *testing.T) {
	t.Parallel()
	response := NewSSHRunResponse(RunResult{NativeChange: &NativeChange{ReusedDispositionAttemptID: "attempt_source"}, TurnStarted: true, TurnCount: 2, Tokens: TokenTotals{TotalTokens: 19}, Compute: &compute.Usage{CPUSeconds: 3, AvgMemoryBytes: 1e9, WallSeconds: 4, ComputeUSD: .001}}, io.ErrUnexpectedEOF)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var got SSHRunResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Result.Compute == nil || *got.Result.Compute != *response.Result.Compute || !got.Result.TurnStarted || got.Result.Tokens.TotalTokens != 19 || got.Result.NativeChange == nil || got.Result.NativeChange.ReusedDispositionAttemptID != "attempt_source" || got.Err() == nil {
		t.Fatalf("lost failure result: %+v", got)
	}
}

func TestResolveSSHWorkflowPreservesHookShell(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "bash", "sh", "pwsh"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workflow, err := config.ParseWorkflow([]byte(fmt.Sprintf("---\ntracker:\n  kind: hub_native\nhooks:\n  shell: %q\n---\nPrompt\n", name)))
			if err != nil {
				t.Fatal(err)
			}
			forwarded, err := ResolveSSHWorkflow(t.Context(), workflow)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(forwarded)
			if err != nil {
				t.Fatal(err)
			}
			var received config.Workflow
			if err := json.Unmarshal(data, &received); err != nil {
				t.Fatal(err)
			}
			if received.Config.Hooks.Shell != name || workflow.Config.Hooks.Shell != name {
				t.Fatalf("hook shell changed during forwarding: source %q, received %q, want %q", workflow.Config.Hooks.Shell, received.Config.Hooks.Shell, name)
			}
		})
	}
}

type sshReleaseDeliveryTestExecution struct {
	*readToolTestExecution
	result ReleaseDeliveryResult
	refuse bool
}

func (e *sshReleaseDeliveryTestExecution) ReconcileReleaseDelivery(ctx context.Context, attempt int64, generation uint64) (ReleaseDeliveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ReleaseDeliveryResult{}, err
	}
	if attempt != 7 || generation != 11 {
		return ReleaseDeliveryResult{}, errors.New("release delivery lost attempt identity")
	}
	if e.refuse {
		return ReleaseDeliveryResult{}, ErrExecutionAuthorityUnavailable
	}
	return e.result, nil
}

func TestSSHReleaseDeliveryReconciliation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		settled bool
		message string
		refuse  bool
	}{
		{name: "open delivery continues"},
		{name: "delivered source settles", settled: true, message: "Source verified on release branch"},
		{name: "closed delivery retains human action", settled: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Reopen the delivery PR\n```"},
		{name: "expired authority refuses reconciliation", refuse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner := &sshReleaseDeliveryTestExecution{readToolTestExecution: &readToolTestExecution{}, result: ReleaseDeliveryResult{Settled: test.settled, Result: RunResult{FinalMessage: test.message}}, refuse: test.refuse}
			request := RunRequest{Execution: owner}
			wire := NewSSHRunRequest(request)
			wire.Native = true
			left, right := net.Pipe()
			t.Cleanup(func() { left.Close(); right.Close() })
			central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(request, nil, nil, nil))
			t.Cleanup(central.Close)
			remote := NewSSHPeer(t.Context(), right, right, nil)
			t.Cleanup(remote.Close)
			execution := wire.Bind(t.Context(), remote).Execution.(ReleaseDeliveryExecution)
			result, err := execution.ReconcileReleaseDelivery(t.Context(), 7, 11)
			if test.refuse {
				if !errors.Is(err, ErrExecutionAuthorityUnavailable) {
					t.Fatalf("release delivery lost authority refusal: %v", err)
				}
				return
			}
			if err != nil || result.Settled != test.settled || result.Result.FinalMessage != test.message {
				t.Fatalf("release delivery lost settlement: %+v, %v", result, err)
			}
		})
	}
}

type sshSourceIterationTestExecution struct {
	*readToolTestExecution
	iterations int
}

func (*sshSourceIterationTestExecution) SourceReview(context.Context) (NativeChange, error) {
	return NativeChange{ChangeID: "change-fixture", VersionID: "version-fixture", HeadSHA: "head-fixture"}, nil
}

func (e *sshSourceIterationTestExecution) PrepareSourceIteration(_ context.Context, source tracker.NativeChangeReference) error {
	if source.ChangeID != "change-fixture" || source.VersionID != "version-fixture" || source.HeadSHA != "head-fixture" {
		return errors.New("source iteration lost its reviewed version")
	}
	e.iterations++
	return nil
}

func TestSSHWorkerSourceIterationCompatibility(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy controller=%t", legacy), func(t *testing.T) {
			t.Parallel()
			owner := &sshSourceIterationTestExecution{readToolTestExecution: &readToolTestExecution{}}
			request := RunRequest{Execution: owner}
			wire := NewSSHRunRequest(request)
			wire.Native = true
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				var bootstrap map[string]json.RawMessage
				if err := json.Unmarshal(data, &bootstrap); err != nil {
					t.Fatal(err)
				}
				delete(bootstrap, "SourceIteration")
				data, err = json.Marshal(bootstrap)
				if err != nil {
					t.Fatal(err)
				}
			}
			var transported SSHRunRequest
			if err := json.Unmarshal(data, &transported); err != nil {
				t.Fatal(err)
			}
			left, right := net.Pipe()
			t.Cleanup(func() { left.Close(); right.Close() })
			callback := NewSSHCallbackHandler(request, nil, nil, nil)
			central := NewSSHPeer(t.Context(), left, left, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
				if legacy && (method == "execution.SourceReview" || method == "execution.PrepareSourceIteration") {
					return nil, errors.New("unsupported SSH execution method")
				}
				return callback(ctx, method, args)
			})
			t.Cleanup(central.Close)
			remote := NewSSHPeer(t.Context(), right, right, nil)
			t.Cleanup(remote.Close)
			bound := transported.Bind(t.Context(), remote)
			if err := bound.Execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: RoleCode, Backend: "codex", Model: "fixture"}); err != nil {
				t.Fatal(err)
			}
			iteration, supported := bound.Execution.(NativeSourceIterationExecution)
			if supported == legacy {
				t.Fatalf("source iteration supported = %t for legacy controller = %t", supported, legacy)
			}
			if supported {
				change, err := iteration.SourceReview(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := iteration.PrepareSourceIteration(t.Context(), tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: change.VersionID, HeadSHA: change.HeadSHA}); err != nil {
					t.Fatal(err)
				}
			}
			if err := bound.Execution.(CompletionExecution).PrepareFinish(t.Context(), "succeeded", "worker turn completed", nil); err != nil {
				t.Fatal(err)
			}
			if !owner.started || owner.completionBody != "worker turn completed" || (!legacy && owner.iterations != 1) || (legacy && owner.iterations != 0) {
				t.Fatalf("worker did not complete through compatible callbacks: %+v", owner)
			}
		})
	}
}
