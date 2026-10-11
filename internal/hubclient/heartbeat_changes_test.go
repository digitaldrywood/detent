package hubclient

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

func TestHeartbeatDeltaApplication(t *testing.T) {
	start := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	item := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_delta", Number: 1, Revision: 1}, Title: "initial", State: "Todo", LastActivityAt: start}
	lists, details := 0, 0
	fail := false
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodPost:
			result = item
		case strings.HasSuffix(r.URL.Path, "/work-items"):
			lists++
			result = tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{item}}
		default:
			details++
			if fail {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.WriteString(`{"code":"unavailable"}`)
				return w.Result(), nil
			}
			result = item
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	c := &NativeConnector{client: native}
	for _, test := range []struct {
		name      string
		reset     bool
		changed   bool
		write     bool
		fail      bool
		fallback  bool
		wantLists int
		wantReads int
		wantTitle string
	}{
		{name: "initial reset", reset: true, wantLists: 1, wantTitle: "initial"},
		{name: "unchanged", wantLists: 1, wantTitle: "initial"},
		{name: "changed item", changed: true, wantLists: 1, wantReads: 1, wantTitle: "changed item"},
		{name: "failed hydration", changed: true, fail: true, wantLists: 1, wantReads: 2},
		{name: "retry retained delta", wantLists: 1, wantReads: 3, wantTitle: "failed hydration"},
		{name: "cursor reset", reset: true, wantLists: 2, wantReads: 3, wantTitle: "failed hydration"},
		{name: "reset acknowledged once", wantLists: 2, wantReads: 3, wantTitle: "failed hydration"},
		{name: "read own mutation", write: true, wantLists: 2, wantReads: 4, wantTitle: "read own mutation"},
		{name: "legacy fallback", fallback: true, wantLists: 3, wantReads: 4, wantTitle: "read own mutation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := native.changeCursor()
			changes := &runnerauth.HeartbeatChanges{Cursor: test.name, Reset: test.reset, CapabilitiesDigest: "digest"}
			if test.changed || test.write {
				item.Title, item.Revision, item.LastActivityAt = test.name, item.Revision+1, item.LastActivityAt.Add(time.Second)
			}
			if test.changed {
				changes.Items = []runnerauth.HeartbeatItem{{ID: item.WorkItemID, Revision: item.Revision, LastActivityAt: item.LastActivityAt}}
			}
			if test.fallback {
				changes = nil
			}
			native.applyHeartbeatChanges(changes)
			if test.write {
				if _, err := native.Dependency(t.Context(), item.WorkItemID, tracker.DependencyMutation{}); err != nil {
					t.Fatal(err)
				}
			}
			fail = test.fail
			issues, err := c.FetchIssuesByStates(t.Context(), []string{"Todo"})
			if test.fail {
				if err == nil || native.changeCursor() != previous {
					t.Fatalf("failed delta acknowledged: cursor=%q error=%v", native.changeCursor(), err)
				}
			} else if err != nil || len(issues) != 1 || issues[0].Title != test.wantTitle {
				t.Fatalf("issues=%+v error=%v", issues, err)
			}
			if lists != test.wantLists || details != test.wantReads {
				t.Fatalf("lists/details=%d/%d want=%d/%d", lists, details, test.wantLists, test.wantReads)
			}
		})
	}
}

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
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"native": "prj_test"}, Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", Capacity: capacity, Version: "test"}, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, Now: func() time.Time { return now }})
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
				for _, s := range schedulers {
					s.client.capabilitiesMu.Lock()
					s.client.capabilitiesAt = time.Now().Add(-time.Minute)
					s.client.capabilitiesMu.Unlock()
				}
				cycle()
			}
			total, heartbeats := 0, 0
			for path, count := range counts {
				total += count
				if strings.HasSuffix(path, "/heartbeat") {
					heartbeats += count
				}
			}
			if heartbeats != 20 || mode == "delta" && total != 20 || mode != "delta" && total <= 20 {
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
				{"approved policy ID", func() { approved.SourceDigest = policy.Digest([]byte("new-source")); approved = approved.WithID() }, "/policy"},
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
					if reads != 2 {
						t.Fatalf("changed resource reads=%d want=2 routes=%+v", reads, counts)
					}
				})
			}
		})
	}
}

func TestHeartbeatClaimHints(t *testing.T) {
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		changes   *runnerauth.HeartbeatChanges
		freed     bool
		want      bool
		wantAgain bool
	}{
		{name: "legacy", want: true, wantAgain: true},
		{name: "idle", changes: &runnerauth.HeartbeatChanges{Cursor: "idle"}},
		{name: "claimable", changes: &runnerauth.HeartbeatChanges{Cursor: "ready", Claimable: true}, want: true, wantAgain: true},
		{name: "slot freed", changes: &runnerauth.HeartbeatChanges{Cursor: "idle"}, freed: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			native.applyHeartbeatChanges(test.changes)
			if test.freed {
				native.slotFreed()
			}
			if first, second := native.claimHint(), native.claimHint(); first != test.want || second != test.wantAgain {
				t.Fatalf("claim permission=%v/%v want=%v/%v", first, second, test.want, test.wantAgain)
			}
		})
	}
}
