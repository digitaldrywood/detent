package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRetiredControllerCannotOverwriteReplacementWithAcknowledgment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		update   bool
		inFlight bool
	}{
		{name: "project configuration prepared before retirement"},
		{name: "project configuration in flight during retirement", inFlight: true},
		{name: "update prepared before retirement", update: true},
		{name: "update in flight during retirement", update: true, inFlight: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "identity.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_test"
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
			file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			defer func() { releaseOnce.Do(func() { close(release) }); workers.Wait() }()
			barrier := func() { close(entered); <-release }
			projectRequest := &runnerauth.ProjectConfigurationRequest{RequestID: "project-request", ProjectID: "prj_test", Operation: "apply_local_project_policy"}
			updateRequest := &runnerauth.UpdateRequest{RequestedAt: now, ID: "update-request", Service: "detent", ExpectedBuildRevision: strings.Repeat("a", 64), Version: "1.0.1", Release: true}
			snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1}.Normalized()}
			if test.update {
				snapshot.Routing.UpdateRequest = updateRequest
			} else {
				snapshot.ProjectConfigurationRequest = projectRequest
			}
			type report struct {
				Version string                           `json:"version"`
				Project *runnerauth.ProjectConfiguration `json:"project_configuration"`
				Update  *runnerauth.UpdateObservation    `json:"update"`
			}
			var latest atomic.Pointer[report]
			var reports atomic.Int64
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				var response any
				switch {
				case strings.HasSuffix(request.URL.Path, "/runners/"+file.Identity.RunnerID):
					response = file.Identity
				case request.URL.Path == "/api/v2/capabilities":
					response = map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeRunnerUpdateCapability, tracker.NativeProjectConfigurationCapability}}
				case strings.HasSuffix(request.URL.Path, "/projects/prj_test"):
					response = tracker.NativeProject{Profile: "native"}
				case strings.HasSuffix(request.URL.Path, "/heartbeat"):
					var received report
					if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
						return nil, err
					}
					reports.Add(1)
					acknowledgment := received.Project != nil && received.Project.RequestID != "" || received.Update != nil && received.Update.Receipt != nil
					if test.inFlight && acknowledgment {
						barrier()
					}
					latest.Store(&received)
					response = snapshot
				default:
					return nil, errors.New("unexpected request: " + request.URL.Path)
				}
				data, err := json.Marshal(response)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), Request: request}, nil
			})
			client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			source, err := NewScheduler(client, SchedulerConfig{
				OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"local": "prj_test"},
				Machine: Machine{ID: file.Identity.MachineID, Hostname: "fixture-host", Capacity: 1, Version: "1.0.0"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute, Now: func() time.Time { return now },
				UpdateOwner: func(_ context.Context, desired *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
					observation := &runnerauth.UpdateObservation{Protocol: 1, Service: "detent", Supported: true, Discovery: "available", ObservedAt: now, Running: runnerauth.BuildEvidence{Version: "1.0.0", Commit: "none", Source: "unknown", OS: "linux", Architecture: "amd64", ObservedAt: now}}
					observation.Revision = observation.BuildRevision()
					if desired != nil {
						if !test.inFlight {
							barrier()
						}
						observation.Receipt = &runnerauth.UpdateReceipt{Request: *desired, Status: "refused", ObservedAt: now}
					}
					return observation
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			source.SetProjectConfigurationOwner(func(_ context.Context, project string, desired *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration {
				view := runnerauth.ProjectConfiguration{ProjectID: project, Authority: "local_global_configuration", Source: "unavailable", ObservedAt: now}
				if desired != nil {
					if !test.inFlight {
						barrier()
					}
					view.RequestID = desired.RequestID
				}
				return view
			})
			reported := make(chan error, 1)
			workers.Go(func() { reported <- source.Heartbeat(t.Context()) })
			select {
			case <-entered:
			case err := <-reported:
				t.Fatalf("heartbeat exited before the acknowledgment barrier: %v", err)
			}
			wantReports := reports.Load()
			var resume func()
			if test.inFlight {
				if source.controlMu.TryLock() {
					source.controlMu.Unlock()
					t.Fatal("in-flight acknowledgment is outside controller retirement synchronization")
				}
				paused := make(chan func(), 1)
				workers.Go(func() { paused <- source.PauseController() })
				testenv.WaitUntil(t, t.Context(), func() error {
					select {
					case <-paused:
						t.Fatal("controller retired before its acknowledgment completed")
					default:
					}
					if source.controlMu.TryRLock() {
						source.controlMu.RUnlock()
						return errors.New("retirement has not reached the in-flight acknowledgment")
					}
					return nil
				})
				releaseOnce.Do(func() { close(release) })
				resume = <-paused
			} else {
				resume = source.PauseController()
			}
			defer resume()
			replacement := &report{Version: "1.0.1", Update: &runnerauth.UpdateObservation{Running: runnerauth.BuildEvidence{Version: "1.0.1"}}}
			latest.Store(replacement)
			releaseOnce.Do(func() { close(release) })
			if err := <-reported; err != nil {
				t.Fatal(err)
			}
			if err := source.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := latest.Load()
			if reports.Load() != wantReports || got.Version != "1.0.1" || got.Update == nil || got.Update.Running.Version != "1.0.1" {
				t.Fatalf("retired acknowledgment overwrote the replacement: reports=%d want=%d latest=%+v", reports.Load(), wantReports, got)
			}
		})
	}
}
