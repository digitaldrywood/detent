package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type sessionCallbackExecution struct {
	runner.Execution
	lease tracker.NativeLease
}

func (e sessionCallbackExecution) Recovery() tracker.NativeRecovery {
	return tracker.NativeRecovery{Lease: e.lease}
}

func sessionCallbackClient(t *testing.T, send executionRoundTrip) *RunnerSessions {
	t.Helper()
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: send}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	return NewRunnerSessions(native)
}

func TestRevokedLocalSessionCallback(t *testing.T) {
	const child = "DETENT_TEST_LOCAL_SESSION_CALLBACK"
	if os.Getenv(child) == "1" {
		peer := runner.NewSSHPeer(t.Context(), os.Stdin, os.Stdout, nil)
		var id int64
		_ = peer.Call(t.Context(), "store.StartSession", &id, store.SessionStart{})
		<-peer.Context().Done()
		return
	}
	for _, cause := range []error{runner.ErrOperatorStopped, runner.ErrExecutionAuthorityUnavailable} {
		t.Run(cause.Error(), func(t *testing.T) {
			var posts atomic.Int32
			sessions := sessionCallbackClient(t, func(r *http.Request) (*http.Response, error) {
				posts.Add(1)
				rec := httptest.NewRecorder()
				err := json.NewEncoder(rec).Encode(runnersession.Snapshot{ID: 1, Revision: 1})
				return rec.Result(), err
			})
			run, err := runner.NewRunner(runner.Dependencies{Workspace: &workspace.LocalGit{}, AgentBackend: struct{ runner.AgentBackend }{}, Store: sessions})
			if err != nil {
				t.Fatal(err)
			}
			lease := tracker.NativeLease{ID: "lease_example", WorkItemID: "wi_example", FencingToken: 1}
			callbacks := run.LocalRunCallbacks(runner.RunRequest{Execution: sessionCallbackExecution{lease: lease}})
			guard, revoke := context.WithCancelCause(t.Context())
			defer revoke(nil)
			guarded := WithMutationAuthority(guard, sessions.Scope(), lease, func() error { return context.Cause(guard) })
			processCtx, stop := context.WithCancel(context.WithoutCancel(guarded))
			t.Cleanup(stop)
			cmd := exec.CommandContext(processCtx, os.Args[0], "-test.run=^TestRevokedLocalSessionCallback$")
			cmd.Env = append(cmd.Environ(), child+"=1", "GOCOVERDIR="+t.TempDir())
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			procgroup.Configure(processCtx, cmd)
			if err := procgroup.Start(cmd); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = input.Close()
				if err := cmd.Wait(); err != nil {
					t.Error(err)
				}
			})
			identity, err := procgroup.Inspect(cmd)
			if err != nil {
				t.Fatal(err)
			}
			handled := make(chan error, 1)
			peer := runner.NewSSHPeer(processCtx, output, input, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
				revoke(cause)
				result, err := callbacks.Handle(ctx, method, args)
				handled <- err
				return result, err
			})
			select {
			case err := <-handled:
				if !errors.Is(err, cause) || posts.Load() != 0 {
					t.Errorf("callback error=%v posts=%d, want %v and zero posts", err, posts.Load(), cause)
				}
			case <-peer.Context().Done():
				t.Fatal("worker disconnected before its session callback")
			}
			if alive, err := procgroup.Alive(identity); err != nil || !alive || processCtx.Err() != nil {
				t.Fatalf("worker alive=%t error=%v process context=%v", alive, err, processCtx.Err())
			}
		})
	}
}

func TestRunnerSessionBudgetRechecksAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard, revoke := context.WithCancelCause(t.Context())
		defer revoke(nil)
		posts := 0
		sessions := sessionCallbackClient(t, func(r *http.Request) (*http.Response, error) {
			posts++
			rec := httptest.NewRecorder()
			if posts == 1 {
				rec.Header().Set("Retry-After", "2")
				rec.WriteHeader(http.StatusTooManyRequests)
				_, _ = rec.WriteString(`{"code":"runner_request_budget"}`)
				time.AfterFunc(time.Second, func() { revoke(runner.ErrOperatorStopped) })
			} else {
				if err := json.NewEncoder(rec).Encode(runnersession.Snapshot{ID: 1, Revision: 1}); err != nil {
					return nil, err
				}
			}
			return rec.Result(), nil
		})
		ctx := WithMutationAuthority(context.WithoutCancel(guard), sessions.Scope(), tracker.NativeLease{ID: "lease_example", FencingToken: 1}, func() error { return context.Cause(guard) })
		_, err := sessions.StartSession(ctx, store.SessionStart{})
		if !errors.Is(err, runner.ErrOperatorStopped) || posts != 1 {
			t.Fatalf("budget replay error=%v posts=%d, want operator stop and only the refused request", err, posts)
		}
	})
}
