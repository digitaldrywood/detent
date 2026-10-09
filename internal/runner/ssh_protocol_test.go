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
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestSSHErrorRoundTrip(t *testing.T) {
	t.Parallel()
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
	remoteExecution.setPublicationIdentity(func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error) {
		return identity, nil
	})
	var privateOwner sync.Mutex
	for _, mode := range []string{"confirmed", "wrong head", "wrong base", "wrong repository", "wrong branch", "wrong version", "stale invocation", "merge", "cancelled", "lost authority", "ambiguous", "disconnect"} {
		t.Run("publication/"+mode, func(t *testing.T) {
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
				if kind == "pr_create" && state == "confirmed" && value.External.ID != "7" {
					t.Error("PR identity was dropped")
				}
				return nil
			}}
			remoteExecution.SetPublicationSource(func(ctx context.Context, received tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
				if received.ID != version.ID || opts.Repository != version.Repository || opts.HeadSHA != version.HeadSHA || opts.TargetBranch != "develop" || opts.Authorize == nil || opts.PublicationEffect == nil || opts.GitHubClient != nil {
					return workspace.GitHubPublication{}, errors.New("publication forwarding lost exact binding or leaked a client")
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
			if mode == "confirmed" {
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
	response := NewSSHRunResponse(RunResult{TurnStarted: true, TurnCount: 2, Tokens: TokenTotals{TotalTokens: 19}, Compute: &compute.Usage{CPUSeconds: 3, AvgMemoryBytes: 1e9, WallSeconds: 4, ComputeUSD: .001}}, io.ErrUnexpectedEOF)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var got SSHRunResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Result.Compute == nil || *got.Result.Compute != *response.Result.Compute || !got.Result.TurnStarted || got.Result.Tokens.TotalTokens != 19 || got.Err() == nil {
		t.Fatalf("lost failure result: %+v", got)
	}
}
