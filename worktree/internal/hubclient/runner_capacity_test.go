package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func TestRunnerCapacityHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	for _, test := range []struct {
		name              string
		supported         bool
		denied            bool
		dispatch          bool
		draining          bool
		retryUpdate       bool
		reenterControl    bool
		overlapControl    bool
		precedingDispatch bool
		storagePressure   bool
	}{
		{name: "older Hub"},
		{name: "capacity owner Hub", supported: true},
		{name: "storage pressure preserves configuration and recovers", supported: true, storagePressure: true},
		{name: "dispatch heartbeat", supported: true, dispatch: true},
		{name: "denied heartbeat", supported: true, denied: true},
		{name: "heartbeat upgrade stops claims", supported: true, dispatch: true, draining: true},
		{name: "failed critical Hub update retries while drained", supported: true, dispatch: true, draining: true, retryUpdate: true},
		{name: "configuration control permits candidate heartbeat", supported: true, reenterControl: true},
		{name: "configuration receipt preserves routing order", supported: true, overlapControl: true},
		{name: "configuration observation follows dispatch heartbeat", supported: true, precedingDispatch: true},
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
			staleProject := &runnerauth.ProjectConfigurationRequest{RequestID: "cached-project-request", ProjectID: "prj_test", Operation: "apply_local_project_policy"}
			stale := runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("b", 64), Capacity: 4, Backend: "codex"}
			if err := runnerauth.SaveRoutingCache(path, runnerauth.RoutingSnapshot{ProjectConfigurationRequest: staleProject, RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 4, CapacityRequest: &stale, UpdateRequest: &runnerauth.UpdateRequest{RequestedAt: time.Now().UTC(), ID: "cached-stale-update", Service: "detent", ExpectedBuildRevision: strings.Repeat("b", 64), Version: "1.2.0", Release: true}}.Normalized()}); err != nil {
				t.Fatal(err)
			}
			request := runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("a", 64), Capacity: 6, Backend: "codex"}
			observedAt := time.Now().UTC()
			updateRequest := runnerauth.UpdateRequest{RequestedAt: observedAt, ID: "update-selected", Service: "detent", ExpectedBuildRevision: strings.Repeat("a", 64), Version: "1.2.4", Release: true}
			updateReport := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "1.2.4", AvailableObservedAt: observedAt, ObservedAt: observedAt, Running: runnerauth.BuildEvidence{Version: "test", Commit: "none", Source: "unknown", OS: "linux", Architecture: "amd64", ObservedAt: observedAt}}
			updateReport.Revision = updateReport.BuildRevision()
			updateCalls, updateApplications, updateRestarts := 0, 0, 0

			projectRequest := &runnerauth.ProjectConfigurationRequest{RequestID: "project-request", ProjectID: "prj_test", Operation: "apply_local_project_policy"}
			projectCalls, projectApplications := 0, 0
			projectReport := runnerauth.ProjectConfiguration{ProjectID: "prj_test", Authority: "local_global_configuration", Source: "unavailable", ObservedAt: observedAt}
			snapshot := runnerauth.RoutingSnapshot{ProjectConfigurationRequest: projectRequest, RunnerID: file.Identity.RunnerID, Revision: 2, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 6, CapacityRequest: &request, UpdateRequest: &updateRequest}.Normalized()}
			if test.draining {
				snapshot.Routing.State = "draining"
				updateRequest.FollowHub = true
				snapshot.Routing.UpdateRequest = &updateRequest
			}
			calls, observations, applications, appliedLimit := 0, 0, 0, 2
			dispatching := test.dispatch
			storageHeld := test.storagePressure
			var reportedCapacities []int
			var observedProject *runnerauth.ProjectConfiguration
			var responseMu sync.Mutex
			candidateEntered := make(chan struct{})
			parentDone := make(chan struct{})
			candidateDone := make(chan error, 1)
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
						features = append(features, tracker.NativeRunnerCapacityCapability, tracker.NativeRunnerUpdateCapability, tracker.NativeProjectConfigurationCapability)
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
						Project       *runnerauth.ProjectConfiguration `json:"project_configuration"`
						Update        *runnerauth.UpdateObservation    `json:"update"`
						Capacity      int                              `json:"capacity"`
						Configuration *runnerauth.CapacityConfig       `json:"capacity_configuration"`
					}
					if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if test.storagePressure {
						reportedCapacities = append(reportedCapacities, report.Capacity)
					}
					expectedCapacity := appliedLimit
					if storageHeld {
						expectedCapacity = 0
					}
					if report.Configuration != nil {
						if err := report.Configuration.Validate(); err != nil {
							t.Errorf("invalid configuration evidence: %v", err)
						}
					}
					if test.supported && !test.overlapControl && (report.Configuration == nil || report.Configuration.RuntimeLimit != appliedLimit || report.Capacity != expectedCapacity) {
						t.Errorf("report=%+v", report)
					}
					if !test.supported && report.Configuration != nil {
						t.Error("sent unsupported configuration authority")
					}
					if test.supported && !test.overlapControl && report.Update == nil || !test.supported && report.Update != nil {
						t.Errorf("update support report=%+v", report.Update)
					}
					if test.supported && !dispatching && !test.overlapControl && report.Project == nil || (!test.supported || dispatching) && report.Project != nil {
						t.Error("incorrect project configuration capability")
					}
					if report.Project != nil {
						observedProject = report.Project
					}
					responseMu.Lock()
					if report.Project != nil && report.Project.RequestID == projectRequest.RequestID {
						snapshot.ProjectConfigurationRequest = nil
					}
					observations++
					response := snapshot
					responseMu.Unlock()
					if test.overlapControl && report.Project == nil && observations == 2 {
						close(candidateEntered)
						select {
						case <-parentDone:
						case <-time.After(time.Second):
						}
					}
					if test.denied {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"code":"access_denied","message":"denied"}`))
						return
					}
					if err := json.NewEncoder(w).Encode(response); err != nil {
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
			var updateScheduler *detentupdate.Scheduler
			if test.retryUpdate {
				updateReport.Running.Version = "1.2.3"
				updateScheduler, err = detentupdate.NewScheduler(detentupdate.SchedulerConfig{
					CheckInterval: 2 * time.Second, StatePath: filepath.Join(root, "update.json"), RunningBuild: updateReport.Running,
					Now:            func() time.Time { return now },
					ReserveIdle:    func(context.Context) (func(), bool) { return func() {}, true },
					ReserveDrain:   func(context.Context) (func(), error) { return func() {}, nil },
					RequestRestart: func(string) bool { updateRestarts++; return true },
					Updater: heartbeatUpdater{apply: func(_ context.Context, options detentupdate.ApplyOptions) (detentupdate.Status, error) {
						updateApplications++
						if !options.FollowHub || options.ExpectedVersion != updateRequest.Version {
							t.Errorf("lost Hub update target: %+v", options)
						}
						if updateApplications == 1 {
							return detentupdate.Status{}, errors.New("release download unavailable")
						}
						return detentupdate.Status{Action: detentupdate.ActionUpdated, LatestVersion: updateRequest.Version, LatestCommit: strings.Repeat("c", 40), BinarySHA256: strings.Repeat("d", 64), VerifiedRelease: true}, nil
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				updateReport = updateScheduler.EnrolledUpdate(t.Context(), updateReport.Running, nil)
				updateRequest.ExpectedBuildRevision = updateReport.Revision
			}
			heartbeatInterval := time.Second
			if test.precedingDispatch {
				heartbeatInterval = 30 * time.Second
			}
			config := SchedulerConfig{
				OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"native": "prj_test"},
				Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", DisplayName: "Runner", Version: "test", Capacity: 2}, HeartbeatInterval: heartbeatInterval, LeaseTTL: time.Minute, Now: func() time.Time { return now },
				UpdateOwner: func(_ context.Context, desired *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
					updateCalls++
					if updateScheduler != nil {
						return updateScheduler.EnrolledUpdate(t.Context(), updateReport.Running, desired)
					}
					if desired != nil {
						if *desired != updateRequest {
							t.Errorf("wrong update delivery=%+v", desired)
						}
						if updateReport.Receipt != nil && updateReport.Receipt.Request == *desired {
							copy := *updateReport
							return &copy
						}
						updateApplications++
						updateReport.Receipt = &runnerauth.UpdateReceipt{Request: *desired, Status: "refused", ObservedAt: observedAt}
					}
					copy := *updateReport
					return &copy
				},
				AvailableCapacity: func(_ context.Context, capacity int) int {
					if storageHeld {
						return 0
					}
					return capacity
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
			}
			if test.overlapControl {
				config.UpdateOwner = nil
				config.CapacityConfiguration = nil
			}
			scheduler, err := NewScheduler(client, config)
			if err != nil {
				t.Fatal(err)
			}
			scheduler.SetProjectConfigurationOwner(func(ownerCtx context.Context, project string, desired *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration {
				projectCalls++
				if project != "prj_test" {
					t.Errorf("foreign owner project: %s", project)
				}
				if desired == nil {
					scheduler.mu.Lock()
					heartbeat := scheduler.nativeHeartbeatMu["prj_test"]
					scheduler.mu.Unlock()
					if heartbeat != nil && !heartbeat.TryLock() {
						t.Error("configuration owner read while the project heartbeat lock is held")
					} else if heartbeat != nil {
						heartbeat.Unlock()
					}
				}
				if desired != nil {
					if test.overlapControl {
						now = now.Add(2 * time.Second)
						go func() {
							candidateDone <- scheduler.ensureNativeMachine(ownerCtx, scheduler.nativeProjects["native"])
						}()
						select {
						case <-candidateEntered:
						case <-time.After(3 * time.Second):
							t.Error("expired candidate heartbeat did not reach the Hub")
						}
					}
					if test.reenterControl {
						ctx, cancel := context.WithTimeout(ownerCtx, time.Second)
						done := make(chan error, 1)
						go func() {
							done <- scheduler.ensureNativeMachine(ctx, scheduler.nativeProjects["native"])
						}()
						select {
						case err := <-done:
							if err != nil {
								t.Errorf("candidate heartbeat during configuration control: %v", err)
							}
						case <-ctx.Done():
							t.Error("configuration control blocked candidate heartbeat")
						}
						cancel()
					}
					if desired.RequestID != projectRequest.RequestID {
						t.Error("cached request applied without authenticated heartbeat")
					}
					projectApplications++
					projectReport.RequestID = desired.RequestID
				}
				projectReport.ObservedAt = now
				return projectReport
			})
			heartbeats := 2
			if test.overlapControl {
				heartbeats = 1
			}
			if test.retryUpdate {
				heartbeats = 4
			}
			for heartbeatIndex := range heartbeats {
				var err error
				if test.precedingDispatch {
					dispatching = true
					if err := scheduler.ensureNativeMachine(t.Context(), scheduler.nativeProjects["native"]); err != nil {
						t.Fatal(err)
					}
					beforeDuplicate := observations
					if err := scheduler.ensureNativeMachine(t.Context(), scheduler.nativeProjects["native"]); err != nil {
						t.Fatal(err)
					}
					if observations != beforeDuplicate {
						t.Fatal("recent dispatch heartbeat was not throttled")
					}
					dispatching = false
				}
				if test.draining {
					var issues []connector.Issue
					issues, err = scheduler.fetchNativeCandidate(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native"}, scheduler.nativeProjects["native"])
					if len(issues) != 0 {
						t.Fatal("claimed new work during heartbeat update")
					}
				} else if test.dispatch {
					err = scheduler.ensureNativeMachine(t.Context(), scheduler.nativeProjects["native"])
				} else {
					err = scheduler.Heartbeat(t.Context())
				}
				if test.overlapControl {
					close(parentDone)
					if candidateErr := <-candidateDone; candidateErr != nil {
						t.Errorf("concurrent candidate heartbeat: %v", candidateErr)
					}
					if current := client.runner.projectConfigurationRequest(); current != nil {
						t.Errorf("older heartbeat resurrected completed configuration request: %s", current.RequestID)
					}
				}
				if test.denied {
					if err == nil {
						t.Fatal("denied heartbeat succeeded")
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if test.precedingDispatch && (observedProject == nil || !observedProject.ObservedAt.Equal(now)) {
					t.Fatalf("dispatch heartbeat prevented a fresh configuration observation: calls=%d report=%+v now=%s", projectCalls, observedProject, now)
				}
				if test.precedingDispatch {
					beforeCalls, beforeReports := projectCalls, observations
					now = now.Add(time.Second)
					if err := scheduler.Heartbeat(t.Context()); err != nil {
						t.Fatal(err)
					}
					if projectCalls != beforeCalls || observations != beforeReports {
						t.Fatal("configuration reporting ignored the configured heartbeat interval")
					}
					now = now.Add(-time.Second)
				}
				if test.overlapControl {
					if observations != 3 || projectApplications != 1 {
						t.Errorf("configuration delivery observations/applications=%d/%d, want 3/1", observations, projectApplications)
					}
					return
				}
				if test.retryUpdate {
					wantApplications := []int{1, 1, 2, 2}[heartbeatIndex]
					if updateApplications != wantApplications {
						t.Fatalf("heartbeat %d update applications=%d, want %d", heartbeatIndex, updateApplications, wantApplications)
					}
				}
				if test.storagePressure {
					storageHeld = false
				}
				now = now.Add(heartbeatInterval)
			}
			if test.storagePressure && (len(reportedCapacities) < 2 || reportedCapacities[0] != 0 || reportedCapacities[len(reportedCapacities)-1] != 6) {
				t.Fatalf("storage capacity did not pause and recover: %v", reportedCapacities)
			}
			if test.precedingDispatch {
				if projectCalls != 3 || projectApplications != 1 {
					t.Fatalf("project calls/applications=%d/%d, want 3/1", projectCalls, projectApplications)
				}
				return
			}
			if test.supported && !test.denied && (calls != 2*heartbeats || applications != heartbeats || scheduler.machine.Capacity != 6) || !test.supported && calls != 0 || test.denied && (calls != 1 || applications != 0 || scheduler.machine.Capacity != 2) {
				t.Fatalf("calls/applications/capacity=%d/%d/%d", calls, applications, scheduler.machine.Capacity)
			}
			wantApplications := 1
			if test.retryUpdate {
				wantApplications = 2
				if updateRestarts != 1 || scheduler.machine.Update == nil || scheduler.machine.Update.Receipt == nil || scheduler.machine.Update.Receipt.Status != "restart_requested" {
					t.Fatalf("recovered update restarts=%d report=%+v", updateRestarts, scheduler.machine.Update)
				}
			}
			if test.supported && !test.denied && (updateCalls != 2*heartbeats || updateApplications != wantApplications) || !test.supported && updateCalls != 0 || test.denied && (updateCalls != 1 || updateApplications != 0) {
				t.Fatalf("update observations/applications=%d/%d", updateCalls, updateApplications)
			}
			if test.dispatch {
				if projectCalls != 0 || projectApplications != 0 {
					t.Fatalf("configuration owner called from dispatch: %d/%d", projectCalls, projectApplications)
				}
			} else if test.supported && !test.denied && (projectCalls != 3 || projectApplications != 1) || !test.supported && projectCalls != 0 || test.denied && (projectCalls != 1 || projectApplications != 0) {
				t.Fatalf("project calls/applications=%d/%d", projectCalls, projectApplications)
			}
			wantObservations := heartbeats
			if test.denied {
				wantObservations = 1
			} else if test.supported {
				wantObservations += wantApplications
				if !test.dispatch {
					wantObservations++
				}
			}
			if observations != wantObservations {
				t.Fatalf("heartbeats=%d, want %d without duplicate update reports", observations, wantObservations)
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

type heartbeatUpdater struct {
	apply func(context.Context, detentupdate.ApplyOptions) (detentupdate.Status, error)
}

func (heartbeatUpdater) Check(context.Context) (detentupdate.Status, error) {
	return detentupdate.Status{Critical: true, LatestVersion: "1.2.4"}, nil
}

func (u heartbeatUpdater) Apply(ctx context.Context, options detentupdate.ApplyOptions) (detentupdate.Status, error) {
	return u.apply(ctx, options)
}
