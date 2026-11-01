package dispatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerHeartbeatIdleTraffic(t *testing.T) {
	for _, mode := range []string{"delta", "legacy", "missing field"} {
		t.Run(mode, func(t *testing.T) {
			counts := map[string]int{}
			files := map[string]runnerauth.File{}
			cursors := map[string]string{}
			acknowledged := map[string]bool{}
			approved := clientTestPolicy()
			digest, version := "stable", "test"
			var delta []runnerauth.HeartbeatItem
			var reset, claimable bool
			items := make([]tracker.NativeIssue, 31)
			for index := range items {
				items[index] = tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_idle%d", index)), Number: index + 1, Revision: 1}, State: "Blocked", Title: "held", LastActivityAt: time.Now().UTC()}
			}
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				path := r.URL.Path
				counts[path]++
				var result any
				switch {
				case strings.HasSuffix(path, "/capabilities"):
					features := []string{"native_issues", "scoped_collaboration", "repository_policy"}
					if mode != "legacy" {
						features = append(features, tracker.NativeHeartbeatChangesCapability)
					}
					result = nativeCapabilities{Version: version, ProtocolMajors: []int{2}, EventSchemas: []int{1}, Features: features}
				case strings.HasSuffix(path, "/heartbeat"):
					parts := strings.Split(path, "/")
					machine := parts[len(parts)-2]
					file := files[machine]
					var request struct {
						Cursor *string `json:"change_cursor"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if mode == "legacy" && request.Cursor != nil || mode != "legacy" && request.Cursor == nil {
						t.Fatalf("wrong cursor negotiation: %+v", request)
					}
					snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 8}.Normalized()}
					if mode == "delta" {
						if *request.Cursor != cursors[machine] && (*request.Cursor != "" || acknowledged[machine]) {
							t.Fatalf("cursor=%q want=%q", *request.Cursor, cursors[machine])
						}
						acknowledged[machine] = *request.Cursor != ""
						cursors[machine] = fmt.Sprintf("cursor-%d", counts[path])
						snapshot.Changes = &runnerauth.HeartbeatChanges{Cursor: cursors[machine], Reset: *request.Cursor == "" || reset, PolicyID: approved.ID, CapabilitiesDigest: digest, Claimable: claimable, Items: delta}
					}
					result = snapshot
				case strings.HasSuffix(path, "/policy"):
					result = policy.Approval{Policy: approved}
				case strings.HasSuffix(path, "/projects/prj_test"):
					result = tracker.NativeProject{ID: "prj_test", Profile: "native"}
				case strings.HasSuffix(path, "/observed"):
					result = struct{}{}
				case strings.HasSuffix(path, "/claims"):
					w.WriteHeader(http.StatusConflict)
					_, _ = w.WriteString(`{"code":"no_claimable_work"}`)
					return w.Result(), nil
				case strings.HasSuffix(path, "/work-items"):
					result = tracker.Page[tracker.NativeIssue]{Items: items}
				case strings.HasSuffix(path, "/work-items/wi_idle0"):
					result = items[0]
				case strings.HasSuffix(path, "/attempts"):
					result = tracker.Page[tracker.NativeAttempt]{}
				case strings.HasSuffix(path, "/history"):
					result = tracker.Page[tracker.CollaborationEvent]{}
				case strings.HasSuffix(path, "/comments"):
					result = tracker.Page[tracker.NativeComment]{}
				default:
					t.Fatalf("unexpected request %s %s", r.Method, path)
				}
				if err := json.NewEncoder(w).Encode(result); err != nil {
					t.Error(err)
				}
				return w.Result(), nil
			})
			now := time.Now()
			var schedulers []*Scheduler
			for _, capacity := range []int{8, 4} {
				path := filepath.Join(t.TempDir(), "private", "identity.json")
				file, err := runnerauth.Initialize(path, "https://hub.test")
				if err != nil {
					t.Fatal(err)
				}
				file.Identity.OrganizationID, file.Identity.ProjectIDs, file.Identity.ExpiresAt = "org_test", []tracker.ProjectID{"prj_test"}, time.Now().Add(24*time.Hour)
				if err := runnerauth.Save(path, file); err != nil {
					t.Fatal(err)
				}
				files[string(file.Identity.MachineID)] = file
				client, err := newTestClient(t, Config{Now: func() time.Time { return now }, URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"native": "prj_test"}, Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", Capacity: capacity, Version: "test"}, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				schedulers = append(schedulers, scheduler)
			}
			cycle := func() {
				t.Helper()
				for _, s := range schedulers {
					if err := s.Heartbeat(t.Context()); err != nil {
						t.Fatal(err)
					}
					if _, err := s.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: approved}); err != nil {
						t.Fatal(err)
					}
					c := s.nativeProjects["native"]
					issues, err := c.FetchIssuesByStates(t.Context(), []string{"Blocked"})
					if err != nil || len(issues) != 31 {
						t.Fatalf("blocked snapshot=%d error=%v", len(issues), err)
					}
					if mode == "delta" {
						if states, err := c.FetchIssueStatesByIDs(t.Context(), []string{"wi_idle0"}); err != nil || len(states) != 1 {
							t.Fatalf("item states=%+v error=%v", states, err)
						}
						if dependencies, err := c.FetchIssueStatesByIdentifiers(t.Context(), []string{"wi_idle0", "prj_test#1"}); err != nil || len(dependencies) != 2 {
							t.Fatalf("dependencies=%+v error=%v", dependencies, err)
						}
					}
					for _, issue := range issues {
						if _, err := c.FetchIssueComments(t.Context(), issue); err != nil {
							t.Fatal(err)
						}
					}
					if got, err := s.client.Version(t.Context()); err != nil || got != version {
						t.Fatalf("Hub version=%q want=%q error=%v", got, version, err)
					}
				}
				now = now.Add(30 * time.Second)
			}
			cycle()
			cycle()
			clear(counts)
			for range 10 {
				now = now.Add(time.Minute)
				cycle()
			}
			total, heartbeats := 0, 0
			for path, count := range counts {
				total += count
				if strings.HasSuffix(path, "/heartbeat") {
					heartbeats += count
				}
			}
			itemReads := counts["/api/v2/organizations/org_test/projects/prj_test/work-items/wi_idle0"]
			if heartbeats != 20 || mode == "delta" && (itemReads != 20 || total != heartbeats+itemReads) || mode != "delta" && total <= 20 {
				t.Fatalf("requests=%d heartbeats=%d routes=%+v", total, heartbeats, counts)
			}
			t.Logf("two runners, 31 Blocked items, 10 idle intervals: %d requests (%d heartbeats)", total, heartbeats)
			if mode != "delta" {
				return
			}
			for _, test := range []struct {
				name   string
				change func()
				route  string
			}{
				{"approved policy ID", func() { approved.SourceDigest = policy.Digest([]byte("new-source")); approved = approved.WithID() }, "/policy/observed"},
				{"capabilities digest", func() { digest, version = "deploy", "new-build" }, "/capabilities"},
				{"newly claimable work", func() { claimable = true }, "/claims"},
				{"item delta", func() {
					claimable = false
					items[0].Title, items[0].Revision, items[0].LastActivityAt = "updated", 2, items[0].LastActivityAt.Add(time.Second)
					delta = []runnerauth.HeartbeatItem{{ID: items[0].WorkItemID, Revision: 2, LastActivityAt: items[0].LastActivityAt}}
				}, "/work-items/wi_idle0"},
				{"reset cursor", func() { delta, reset = nil, true }, "/work-items"},
			} {
				t.Run(test.name, func(t *testing.T) {
					clear(counts)
					test.change()
					cycle()
					reads := 0
					for route, count := range counts {
						if strings.HasSuffix(route, test.route) {
							reads += count
							continue
						}
						if strings.HasSuffix(route, "/work-items") || strings.HasSuffix(route, "/capabilities") || strings.HasSuffix(route, "/claims") {
							t.Fatalf("unchanged resource was polled: %s %d", route, count)
						}
					}
					wantReads := 2
					if test.route == "/work-items/wi_idle0" {
						wantReads += 2
					}
					if reads != wantReads {
						t.Fatalf("changed resource reads=%d want=%d routes=%+v", reads, wantReads, counts)
					}
				})
			}
		})
	}
}
