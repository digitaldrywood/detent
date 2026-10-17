package hubclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestNativeDispatchWaitReconnects(t *testing.T) {
	for _, mode := range []string{"changes", "unchanged", "failure", "unsupported", "held", "long request", "heartbeat replaces wait", "heartbeat wakes dispatch"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				var after []string
				client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					if strings.HasSuffix(r.URL.Path, "/capabilities") {
						features := `"native_dispatch_wait"`
						if mode == "unsupported" {
							features = ""
						}
						response.WriteString(`{"features":[` + features + `]}`)
						return response.Result(), nil
					}
					calls++
					if r.URL.Query().Get("wait") != "30" {
						t.Errorf("wait=%s", r.URL.Query().Get("wait"))
					}
					after = append(after, r.URL.Query().Get("after"))
					if mode == "held" && calls > 1 {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if mode == "long request" {
						deadline, ok := r.Context().Deadline()
						if !ok || time.Until(deadline) != 40*time.Second {
							t.Errorf("poll deadline=%s", time.Until(deadline))
						}
						select {
						case <-time.After(20 * time.Second):
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					if mode == "failure" {
						response.WriteHeader(http.StatusServiceUnavailable)
						response.WriteString(`{"code":"unavailable"}`)
					} else {
						cursor := 1
						if mode == "changes" {
							cursor = calls
						}
						fmt.Fprintf(response, `{"cursor":"generation:%d"}`, cursor)
					}
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "heartbeat replaces wait" || mode == "heartbeat wakes dispatch" {
					native.applyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "heartbeat"})
					client.runner = &runnerCredentialSource{routing: &runnerauth.RoutingSnapshot{Changes: &runnerauth.HeartbeatChanges{Cursor: "heartbeat"}}}
				}
				s := &Scheduler{nativeProjects: map[string]*NativeConnector{"local": {client: native}}}
				wake := make(chan struct{}, 1)
				done := make(chan struct{})
				go func() { defer close(done); s.WaitCandidateChanges(ctx, "local", wake) }()
				synctest.Wait()
				first := len(wake)
				if first > 0 {
					<-wake
				}
				if mode == "heartbeat wakes dispatch" {
					changes := &runnerauth.HeartbeatChanges{Cursor: "new", Claimable: true}
					native.applyHeartbeatChanges(changes)
					client.runner.setRouting(runnerauth.RoutingSnapshot{Changes: changes})
				}
				elapsed := 5 * time.Second
				if mode == "failure" {
					elapsed = 63 * time.Second
				}
				if mode == "long request" {
					elapsed = 21 * time.Second
				}
				time.Sleep(elapsed)
				synctest.Wait()
				wantCalls, wantWake := 6, 1
				switch mode {
				case "unchanged":
					wantWake = 0
				case "failure":
					wantCalls, wantWake = 7, 0
				case "unsupported", "heartbeat replaces wait":
					wantCalls, wantWake = 0, 0
				case "heartbeat wakes dispatch":
					wantCalls, wantWake = 0, 1
				case "held":
					wantCalls, wantWake = 2, 0
				case "long request":
					wantCalls, wantWake = 2, 1
				}
				if calls != wantCalls || len(wake) != wantWake {
					t.Fatalf("calls=%d wakes=%d want=%d/%d", calls, len(wake), wantCalls, wantWake)
				}
				if len(after) > 1 && mode != "failure" && mode != "long request" && after[1] != "generation:1" {
					t.Fatalf("after=%v", after)
				}
				cancel()
				<-done
			})
		})
	}
}
