package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDispatchWaitReconnects(t *testing.T) {
	for _, mode := range []string{"changes", "unchanged", "failure", "unsupported", "held", "long request", "heartbeat replaces wait", "heartbeat wakes dispatch"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				var after []string
				client, err := newTestClient(t, Config{Enrollment: fixtureEnrollment(mode == "heartbeat replaces wait" || mode == "heartbeat wakes dispatch", ""), URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
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
					native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "heartbeat"})
					client.Enrollment().ApplyRouting(runnerauth.RoutingSnapshot{Changes: &runnerauth.HeartbeatChanges{Cursor: "heartbeat"}})
				}
				s := &Scheduler{nativeProjects: map[string]*NativeConnector{"local": fixtureConnector(native)}}
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
					native.ApplyHeartbeatChanges(changes)
					client.Enrollment().ApplyRouting(runnerauth.RoutingSnapshot{Changes: changes})
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

func TestMachineSyncSharesProjectWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var group sync.WaitGroup
		defer func() { cancel(); group.Wait() }()
		path := filepath.Join(t.TempDir(), "private", "identity.json")
		file, err := runnerauth.Initialize(path, "https://hub.example.test")
		if err != nil {
			t.Fatal(err)
		}
		file.Identity.OrganizationID = "org_example"
		file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
		if err := runnerauth.Save(path, file); err != nil {
			t.Fatal(err)
		}
		var requests atomic.Int64
		release := make(chan struct{})
		client, err := newTestClient(t, Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
			response := httptest.NewRecorder()
			if strings.HasSuffix(r.URL.Path, "/capabilities") {
				response.WriteString(`{"features":["native_runner_sync"]}`)
				return response.Result(), nil
			}
			if r.Method != http.MethodPost || r.URL.Path != "/api/v2/organizations/org_example/runners/"+file.Identity.RunnerID+"/sync" {
				return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			var req runnerauth.SyncRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			n := requests.Add(1)
			if req.MachineID != file.Identity.MachineID {
				t.Errorf("wrong machine %q", req.MachineID)
			}
			if n == 1 {
				response.WriteString(`{"versions":{"cursor":"initial"}}`)
			} else {
				if req.WaitSeconds < 24 || req.WaitSeconds > 30 || req.Versions.Cursor != "initial" {
					t.Errorf("unexpected poll %+v", req)
				}
				select {
				case <-release:
					response.WriteString(`{"no_change":true}`)
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			return response.Result(), nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		s := &Scheduler{machine: Machine{ID: file.Identity.MachineID}, nativeProjects: map[string]*NativeConnector{}}
		for _, project := range []string{"prj_one", "prj_two", "prj_three"} {
			native, err := client.Native(file.Identity.OrganizationID, tracker.ProjectID(project))
			if err != nil {
				t.Fatal(err)
			}
			s.nativeProjects[project] = fixtureConnector(native)
		}
		for project := range s.nativeProjects {
			wake := make(chan struct{}, 1)
			group.Go(func() { s.WaitCandidateChanges(ctx, project, wake) })
		}
		synctest.Wait()
		for i := int64(0); i < 10; i++ {
			if got := requests.Load(); got != 2+i {
				t.Fatalf("after %d intervals got %d exchanges", i, got)
			}
			release <- struct{}{}
			synctest.Wait()
		}
	})
}
