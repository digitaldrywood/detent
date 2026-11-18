package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func replayExecution(t *testing.T, send func(*http.Request) (int, string, error)) *nativeExecution {
	t.Helper()
	descriptor := clientTestPolicy()
	lease := tracker.NativeLease{ID: "lease_example", WorkItemID: "wi_example", FencingToken: 1, PolicyID: descriptor.ID}
	client, err := newTestClient(t, Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, body, err := send(r)
		if err != nil {
			return nil, err
		}
		header := make(http.Header)
		header.Set("Retry-After", "90")
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, LeaseTTL: time.Minute, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Minute)}, descriptor, "example")
	e := scheduler.RunExecution(string(lease.WorkItemID)).(*nativeExecution)
	e.data.Identity = &tracker.NativeExecutionIdentity{Role: "review", Backend: "codex", Model: "example"}
	e.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("a"), "initial.go"))
	return e
}

func TestReplayEventRequiresCurrentAuthority(t *testing.T) {
	for _, stage := range []string{"stopped finish", "expiry during diff", "expiry during request budget"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				posts, attempts := 0, 0
				e := replayExecution(t, func(r *http.Request) (int, string, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/diff"):
						if stage == "expiry during diff" {
							<-time.After(time.Minute)
						}
					case strings.HasSuffix(r.URL.Path, "/events"):
						var event tracker.NativeRunEvent
						if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
							return 0, "", err
						}
						attempts++
						if attempts == 1 && stage == "stopped finish" {
							return 0, "", ErrUnavailable
						}
						if attempts == 1 && stage == "expiry during request budget" {
							return 429, `{"code":"runner_request_budget"}`, nil
						}
						posts++
					}
					return 200, "{}", nil
				})
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				guarded := api.WithMutationAuthority(ctx, e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return context.Cause(ctx) })
				want := runner.ErrExecutionAuthorityUnavailable
				var err error
				if stage == "stopped finish" {
					if err := e.Finish(guarded, "succeeded"); !nativeTransportUnavailable(err) {
						t.Fatalf("first finish = %v", err)
					}
					cancel(runner.ErrOperatorStopped)
					want = runner.ErrOperatorStopped
					err = e.Finish(context.WithoutCancel(guarded), "succeeded")
				} else {
					err = e.Checkpoint(guarded, tracker.NativeCheckpoint{HeadSHA: nativeChangeFixtureHead("a"), WorktreeState: "unpushed"})
				}
				if posts != 0 || !errors.Is(err, want) {
					t.Fatalf("event posts=%d error=%v, want zero and %v", posts, err, want)
				}
			})
		})
	}
}
