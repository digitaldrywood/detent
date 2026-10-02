package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapacityHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name      string
		supported bool
		denied    bool
	}{
		{"older Hub", false, false},
		{"capacity owner Hub", true, false},
		{"denied heartbeat", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			stale := runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("b", 64), Capacity: 4, Backend: "codex"}
			if err := runnerauth.SaveRoutingCache(path, runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 4, CapacityRequest: &stale, UpdateRequest: &runnerauth.UpdateRequest{RequestedAt: time.Now().UTC(), ID: "cached-stale-update", Service: "detent", ExpectedBuildRevision: strings.Repeat("b", 64), Version: "1.2.0", Release: true}}.Normalized()}); err != nil {
				t.Fatal(err)
			}
			request := runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("a", 64), Capacity: 6, Backend: "codex"}
			observedAt := time.Now().UTC()
			updateRequest := runnerauth.UpdateRequest{RequestedAt: observedAt, ID: "update-selected", Service: "detent", ExpectedBuildRevision: strings.Repeat("a", 64), Version: "1.2.4", Release: true}
			updateReport := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "1.2.4", AvailableObservedAt: observedAt, ObservedAt: observedAt, Running: runnerauth.BuildEvidence{Version: "test", Commit: "none", Source: "unknown", OS: "linux", Architecture: "amd64", ObservedAt: observedAt}}
			updateReport.Revision = updateReport.BuildRevision()
			updateCalls, updateApplications := 0, 0

			snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 2, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 6, CapacityRequest: &request, UpdateRequest: &updateRequest}.Normalized()}
			calls, observations, applications, appliedLimit := 0, 0, 0, 2
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/runners/"+file.Identity.RunnerID):
					if err := json.NewEncoder(w).Encode(file.Identity); err != nil {
						t.Error(err)
					}
				case r.URL.Path == "/api/v2/capabilities":
					features := []string{"native_issues", "scoped_collaboration", "repository_policy"}
					if test.supported {
						features = append(features, tracker.NativeRunnerCapacityCapability, tracker.NativeRunnerUpdateCapability)
					}
					if err := json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": features}); err != nil {
						t.Error(err)
					}
				case strings.HasSuffix(r.URL.Path, "/projects/prj_test"):
					if err := json.NewEncoder(w).Encode(tracker.NativeProject{Profile: "native"}); err != nil {
						t.Error(err)
					}
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					var report struct {
						Update        *runnerauth.UpdateObservation `json:"update"`
						Capacity      int                           `json:"capacity"`
						Configuration *runnerauth.CapacityConfig    `json:"capacity_configuration"`
					}
					if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if test.supported && (report.Configuration == nil || report.Configuration.RuntimeLimit != appliedLimit || report.Capacity != report.Configuration.RuntimeLimit) {
						t.Errorf("report=%+v", report)
					}
					if !test.supported && report.Configuration != nil {
						t.Error("sent unsupported configuration authority")
					}
					if test.supported && report.Update == nil || !test.supported && report.Update != nil {
						t.Errorf("update support report=%+v", report.Update)
					}
					observations++
					if test.denied {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"code":"access_denied","message":"denied"}`))
						return
					}
					if err := json.NewEncoder(w).Encode(snapshot); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w.Result(), nil
			})
			client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			scheduler, err := NewScheduler(client, SchedulerConfig{
				OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"native": "prj_test"},
				Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", DisplayName: "Runner", Version: "test", Capacity: 2}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute, Now: func() time.Time { return now },
				UpdateOwner: func(_ context.Context, desired *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
					updateCalls++
					if desired != nil {
						if *desired != updateRequest {
							t.Errorf("wrong update delivery=%+v", desired)
						}
						updateApplications++
						updateReport.Receipt = &runnerauth.UpdateReceipt{Request: *desired, Status: "refused", ObservedAt: observedAt}
					}
					copy := *updateReport
					return &copy
				},
				CapacityConfiguration: func(_ context.Context, desired *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
					calls++
					if desired != nil {
						if *desired != request {
							t.Errorf("wrong application request=%+v", desired)
							return nil
						}
						applications++
						appliedLimit = desired.Capacity
					}
					return &runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: appliedLimit, ClientLimit: appliedLimit, RuntimeLimit: appliedLimit, Manageable: true, ObservedAt: now}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				err := scheduler.Heartbeat(t.Context())
				if test.denied {
					if err == nil {
						t.Fatal("denied heartbeat succeeded")
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Second)
			}
			if test.supported && !test.denied && (calls != 4 || applications != 2 || scheduler.machine.Capacity != 6) || !test.supported && calls != 0 || test.denied && (calls != 1 || applications != 0 || scheduler.machine.Capacity != 2) {
				t.Fatalf("calls/applications/capacity=%d/%d/%d", calls, applications, scheduler.machine.Capacity)
			}
			if test.supported && !test.denied && (updateCalls != 3 || updateApplications != 1) || !test.supported && updateCalls != 0 || test.denied && (updateCalls != 1 || updateApplications != 0) {
				t.Fatalf("update observations/applications=%d/%d", updateCalls, updateApplications)
			}
			if observations == 0 {
				t.Fatal("heartbeat was not sent")
			}
			cached, err := runnerauth.LoadRoutingCache(path)
			expected := request
			if test.denied {
				expected = stale
			}
			if err != nil || cached.Routing.CapacityRequest == nil || *cached.Routing.CapacityRequest != expected {
				t.Fatalf("cached request=%+v error=%v", cached, err)
			}
		})
	}
}
