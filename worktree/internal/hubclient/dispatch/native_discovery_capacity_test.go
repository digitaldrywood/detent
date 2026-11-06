package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDiscoveryAvailableCapacity(t *testing.T) {
	for _, test := range []struct {
		name string
		fill func(*runnerauth.Runner)
	}{
		{"host occupied", func(r *runnerauth.Runner) { r.HostUsed = r.HostCapacity }},
		{"runner occupied", func(r *runnerauth.Runner) { r.Used = r.CapacityLimit }},
		{"reported capacity occupied", func(r *runnerauth.Runner) { r.ReportedCapacity = 1; r.Used = 1 }},
		{"project occupied", func(r *runnerauth.Runner) { r.ProjectUsed["prj_test"] = 1 }},
		{"project paused", func(r *runnerauth.Runner) { r.ProjectCapacityLimits["prj_test"] = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "runner.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_test"
			file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			owner := runnerauth.Runner{Binding: file.Identity.Binding, OrganizationID: "org_test", Routing: runnerauth.Routing{CapacityLimit: 4, ProjectCapacityLimits: map[tracker.ProjectID]int{"prj_test": 1}}, HostCapacity: 4, ReportedCapacity: 4, ProjectUsed: map[tracker.ProjectID]int{}}
			test.fill(&owner)
			previews, readiness, claims := make(chan struct{}, 4), make(chan struct{}, 4), make(chan struct{}, 4)
			client, err := newTestClient(t, Config{URL: file.HubURL, Enrollment: fixtureEnrollment(true, path), TokenSource: func() string { return "fixture" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/validate"), strings.HasSuffix(r.URL.Path, "/routing"):
					_ = json.NewEncoder(w).Encode(owner)
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					_, _ = w.Write([]byte(`{"protocol_majors":[2],"features":["native_issues","scoped_collaboration"]}`))
				case strings.HasSuffix(r.URL.Path, "/claims/preview"):
					previews <- struct{}{}
					_, _ = w.Write([]byte(`{"items":[{"work_item_id":"wi_next","state":"Todo"}]}`))
				case strings.HasSuffix(r.URL.Path, "/claims"):
					claims <- struct{}{}
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"code":"no_claimable_work"}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))})
			if err != nil {
				t.Fatal(err)
			}
			client.Enrollment().ApplyRouting(runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Routing: runnerauth.Routing{State: "active"}})
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			scheduler := &Scheduler{client: client, now: func() time.Time { return now }, sessionID: func() (string, error) { return "session", nil }, machine: Machine{Capacity: 4}, heartbeatInterval: time.Minute, nativeHeartbeats: map[tracker.ProjectID]time.Time{"prj_test": now}}
			request := orchestrator.SchedulingRequest{ProjectID: "local", AdmissionLimit: 4, CandidateLimit: 100, NativeLandingBatch: true, CandidateReady: func(context.Context, connector.Issue) bool { readiness <- struct{}{}; return true }}
			for _, full := range []bool{true, false} {
				if !full {
					owner.HostUsed = 0
					owner.Used = 0
					owner.ProjectUsed["prj_test"] = 0
					owner.ProjectCapacityLimits["prj_test"] = 1
				}
				native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "current", Claimable: true})
				issues, err := scheduler.fetchNativeCandidate(t.Context(), request, fixtureConnector(native))
				if err != nil || len(issues) != 0 {
					t.Fatalf("fetch: issues=%v err=%v", issues, err)
				}
				want := 1
				if full {
					want = 0
				}
				for name, signal := range map[string]chan struct{}{"preview": previews, "readiness": readiness, "claim": claims} {
					if len(signal) != want {
						t.Errorf("full=%t %s calls=%d want=%d", full, name, len(signal), want)
					}
					for len(signal) > 0 {
						<-signal
					}
				}
				if full && !native.ClaimHint() {
					t.Error("capacity deferral cleared claim hint")
				}
			}
		})
	}
}
