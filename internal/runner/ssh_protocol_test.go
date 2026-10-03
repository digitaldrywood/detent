package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/compute"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
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
	central := NewSSHPeer(t.Context(), a, a, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		if method != "echo" {
			return nil, errors.New("unexpected callback")
		}
		var value int
		err := json.Unmarshal(args[0], &value)
		return value, err
	})
	t.Cleanup(central.Close)
	var remote *SSHPeer
	ready := make(chan struct{})
	remote = NewSSHPeer(t.Context(), b, b, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		<-ready
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
