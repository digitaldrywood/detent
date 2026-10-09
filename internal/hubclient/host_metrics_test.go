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
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestHostMetricsShutdownHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure string
		elapsed time.Duration
		cached  bool
	}{
		{"older Hub ignores optional metrics", "", 0, false},
		{"unreachable Hub exits at deadline", "timeout", 3 * time.Second, false},
		{"failed send dropped without retry", "unavailable", 0, false},
		{"preserves negotiated payload and closed availability", "", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				if err := os.Chmod(root, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "identity.json")
				file, err := runnerauth.Initialize(path, "http://127.0.0.1:1")
				if err != nil {
					t.Fatal(err)
				}
				file.Identity.OrganizationID = "org_test"
				file.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
				file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
				if err := runnerauth.Save(path, file); err != nil {
					t.Fatal(err)
				}
				calls := 0
				capacity := 2
				if test.cached {
					capacity = 0
				}
				var logs bytes.Buffer
				previousLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				defer slog.SetDefault(previousLogger)
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					calls++
					if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/machines/"+string(file.Identity.MachineID)+"/heartbeat") {
						t.Fatalf("final send performed extra work: %s %s", request.Method, request.URL.Path)
					}
					var payload struct {
						HostMetrics           []hostmetrics.Summary          `json:"host_metrics"`
						Capacity              int                            `json:"capacity"`
						SettingsRejected      bool                           `json:"settings_rejected"`
						Problems              []runnerauth.Problem           `json:"problems"`
						Update                *runnerauth.UpdateObservation  `json:"update"`
						ChangeCursor          *string                        `json:"change_cursor"`
						WorkspaceCapabilities *workspacesession.Capabilities `json:"workspace_capabilities"`
						WorkspaceIsolation    string                         `json:"workspace_isolation"`
					}
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if len(payload.HostMetrics) != 1 || payload.HostMetrics[0].SampleCount != 3 || !payload.HostMetrics[0].Partial || payload.HostMetrics[0].SegmentID.IsZero() {
						t.Fatalf("partial hour missing from final heartbeat: %+v", payload.HostMetrics)
					}
					if payload.Capacity != capacity || !payload.SettingsRejected || len(payload.Problems) != 1 || payload.Problems[0].Code != "keep_awake_failed" {
						t.Fatalf("final heartbeat changed cached runner report: %+v", payload)
					}
					if payload.WorkspaceCapabilities == nil || !payload.WorkspaceCapabilities.Files || payload.WorkspaceIsolation != workspacesession.IsolationUser {
						t.Fatalf("final heartbeat lost workspace report: %+v", payload)
					}
					if test.cached {
						if payload.ChangeCursor == nil || *payload.ChangeCursor != "cursor_test" {
							t.Fatalf("final heartbeat lost negotiated cursor: %+v", payload)
						}
					} else if payload.ChangeCursor != nil {
						t.Fatal("final heartbeat included an unnegotiated cursor")
					}
					if payload.Update != nil {
						t.Fatal("final heartbeat included an unnegotiated update observation")
					}
					deadline, ok := request.Context().Deadline()
					if !ok || time.Until(deadline) != 3*time.Second {
						t.Fatalf("final heartbeat deadline = %v, present = %v", deadline, ok)
					}
					if test.failure == "timeout" {
						<-request.Context().Done()
						return nil, request.Context().Err()
					}
					if test.failure == "unavailable" {
						return nil, errors.New("Hub unreachable")
					}
					return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				client.runner.problems = []runnerauth.Problem{runnerauth.NewProblem("keep_awake_failed")}
				client.runner.settingsRejected = true
				scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: file.Identity.OrganizationID, NativeProjects: map[string]tracker.ProjectID{"test": "prj_test"}, Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
					UpdateOwner: func(context.Context, *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
						t.Fatal("shutdown invoked update owner")
						return nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				scheduler.machine.WorkspaceCapabilities = workspacesession.Capabilities{Files: true}
				scheduler.machine.WorkspaceIsolation = workspacesession.IsolationUser
				if test.cached {
					client.runner.heartbeat = machineHeartbeatPayload(scheduler.machine, nil, false)
					cursor := "cursor_test"
					client.runner.heartbeat.ChangeCursor = &cursor
					client.runner.heartbeat.Capacity = 0
					client.runner.heartbeatPath = "/api/v2/organizations/org_test/projects/prj_test/machines/" + string(file.Identity.MachineID) + "/heartbeat"
					scheduler.machine.Update = &runnerauth.UpdateObservation{Protocol: 999}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				go func() { scheduler.RunHostMetrics(ctx, root, time.Now()); close(done) }()
				synctest.Wait()
				time.Sleep(65 * time.Second)
				stopped := time.Now()
				cancel()
				<-done
				if elapsed := time.Since(stopped); elapsed != test.elapsed || calls != 1 {
					t.Fatalf("shutdown elapsed = %v, calls = %d; want %v and one send", elapsed, calls, test.elapsed)
				}
				wantLogs := 0
				if test.failure != "" {
					wantLogs = 1
				}
				if got := strings.Count(logs.String(), "final runner host metrics heartbeat failed"); got != wantLogs {
					t.Fatalf("failure logged %d times, want %d", got, wantLogs)
				}
			})
		})
	}
}
