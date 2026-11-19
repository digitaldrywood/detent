package workpage_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkListLaneSummariesCountTheFilteredPopulation(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, nil, "", "work-lanes")
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "work-lanes-other")
	other.Create(t, "other project item")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.Enroll(t)
	hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
	provider := hubfixture.CapacityReport(time.Now())
	provider.MaxConcurrent = 2
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{
		"backend_isolation": r.Redemption.BackendIsolation, "display_name": "runner", "capacity": 2, "version": "test", "provider_reports": []providercapacity.Report{provider},
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: time.Now()},
	}), http.StatusOK)
	items := make([]tracker.NativeIssue, 5)
	for i := range items {
		items[i] = createLaneItem(t, f, fmt.Sprintf("lane item %d", i), i)
		createLaneItem(t, other, fmt.Sprintf("other tenant lane item %d", i), i)
	}
	twin := hubfixture.NewNativeInOrganization(t, f.Hub)
	for i := range 3 {
		createLaneItem(t, twin, fmt.Sprintf("twin tenant lane item %d", i), i)
	}
	leases := make([]tracker.NativeLease, 2)
	for i := range leases {
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, items[i], fmt.Sprintf("session-%d", i)))
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hubfixture.Decode(t, response, &leases[i])
		event := hubfixture.StartedEvent(leases[i])
		event.IdempotencyKey = "start-" + string(leases[i].ID)
		path := f.Base + "/work-items/" + string(items[i].WorkItemID) + "/events"
		accepted := f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, event)
		hubfixture.RequireStatus(t, accepted, http.StatusOK)

		replay := f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, event)
		hubfixture.RequireStatus(t, replay, http.StatusOK)
		if replay.Body.String() != accepted.Body.String() {
			t.Fatalf("run.started replay changed the response: got %s, want %s", replay.Body, accepted.Body)
		}
		changed := event
		changed.OccurredAt = time.Now().UTC()
		hubfixture.RequireCode(t, f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, changed), http.StatusConflict, "idempotency_conflict")
	}
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(leases[1].ID), FencingToken: int64(leases[1].FencingToken)}, "cancelled")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	moved := hubfixture.ReadWorkItem(t, f, items[3].WorkItemID, "")
	transition := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(moved.WorkItemID)+"/workflow", f.Token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "lane-progress"}, ExpectedRevision: moved.Revision, State: "In Progress", Reason: "user_requested"})
	hubfixture.RequireStatus(t, transition, http.StatusOK)
	closed := hubfixture.ReadWorkItem(t, f, items[4].WorkItemID, "")
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(closed.WorkItemID)+"/workflow", f.Token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "lane-done"}, ExpectedRevision: closed.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	f.Hub.RefreshBoard(t, f.Project.OrganizationID)
	f.Hub.RefreshBoard(t, twin.Project.OrganizationID)
	want := map[string]tracker.NativeWorkLane{}
	for i, item := range items {
		state := hubfixture.ReadWorkItem(t, f, item.WorkItemID, "").State
		lane := want[state]
		lane.State, lane.Total = state, lane.Total+1
		if i == 0 {
			lane.Running++
		}
		want[state] = lane
	}
	if len(want) < 2 {
		t.Fatalf("fixture lanes=%v want at least two states", want)
	}
	runningState := hubfixture.ReadWorkItem(t, f, items[0].WorkItemID, "").State
	call := hubfixture.MCP(t, f.Hub.Handler(), hubfixture.MCPPath(f.Project.OrganizationID), f.Token)
	lanes := func(lanes []tracker.NativeWorkLane) map[string]tracker.NativeWorkLane {
		result := map[string]tracker.NativeWorkLane{}
		for _, lane := range lanes {
			if _, duplicate := result[lane.State]; duplicate {
				t.Fatalf("duplicate lane %q", lane.State)
			}
			result[lane.State] = lane
		}
		return result
	}
	for _, test := range []struct {
		name   string
		states []string
		want   map[string]tracker.NativeWorkLane
	}{
		{name: "all states", want: want},
		{name: "running state", states: []string{runningState}, want: map[string]tracker.NativeWorkLane{runningState: want[runningState]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := url.Values{"include": {"work"}, "limit": {"1"}, "state": test.states}
			args := map[string]any{"project_id": string(f.Project.ID), "include": []string{"work"}, "limit": 1}
			if len(test.states) > 0 {
				args["states"] = test.states
			}
			pages := 0
			for {
				response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?"+params.Encode(), f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				hubfixture.Decode(t, response, &page)
				var tool operatortool.WorkReadResult[operatortool.NativeWorkPage]
				if err := json.Unmarshal(hubfixture.MCPData(t, call("tools/call", operatortool.WorkList, args), false), &tool); err != nil {
					t.Fatal(err)
				}
				if page.Work == nil || page.Work.Items == nil || tool.Data.Work == nil || page.Total != -1 || tool.Data.Total != -1 {
					t.Fatalf("page %d: rest work=%+v total=%d; mcp work=%+v total=%d", pages, page.Work, page.Total, tool.Data.Work, tool.Data.Total)
				}
				if got := lanes(page.Work.Lanes); !maps.Equal(got, test.want) {
					t.Fatalf("page %d: rest lanes=%v want=%v", pages, got, test.want)
				}
				if got := lanes(tool.Data.Work.Lanes); !maps.Equal(got, test.want) {
					t.Fatalf("page %d: mcp lanes=%v want=%v", pages, got, test.want)
				}
				if len(page.Items) != 1 || len(tool.Data.Items) != 1 || page.Items[0].WorkItemID != tool.Data.Items[0].WorkItemID || (page.NextCursor == "") != (tool.Data.NextCursor == "") {
					t.Fatalf("page %d: rest and mcp selections differ", pages)
				}
				pages++
				if page.NextCursor == "" {
					break
				}
				if pages > len(items) {
					t.Fatal("cursor did not advance")
				}
				params.Set("cursor", page.NextCursor)
				args["cursor"] = tool.Data.NextCursor
			}
		})
	}
	t.Run("without work", func(t *testing.T) {
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?limit=1", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page map[string]json.RawMessage
		hubfixture.Decode(t, response, &page)
		if _, present := page["work"]; present {
			t.Fatalf("plain list computed a work summary: %s", page["work"])
		}
	})
	listed := func(t *testing.T, fixture hubfixture.Native, params url.Values) map[string]tracker.NativeWorkLane {
		params = maps.Clone(params)
		params.Set("limit", "2")
		result := map[string]tracker.NativeWorkLane{}
		for pages := 0; ; pages++ {
			response := f.Hub.Request(t, http.MethodGet, fixture.Base+"/work-items?"+params.Encode(), fixture.Token, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page tracker.NativeIssuePage
			hubfixture.Decode(t, response, &page)
			for _, issue := range page.Items {
				lane := result[issue.State]
				lane.State, lane.Total = issue.State, lane.Total+1
				if issue.WorkItemID == items[0].WorkItemID {
					lane.Running++
				}
				result[issue.State] = lane
			}
			if page.NextCursor == "" || pages > 20 {
				return result
			}
			params.Set("cursor", page.NextCursor)
		}
	}
	created := hubfixture.FormatTime(items[0].CreatedAt.Add(-time.Hour))
	for _, test := range []struct {
		name   string
		params url.Values
		args   map[string]any
	}{
		{"label on every item", url.Values{"label": {"lane-shared"}}, map[string]any{"labels": []string{"lane-shared"}}},
		{"one label", url.Values{"label": {"lane-group-0"}}, map[string]any{"labels": []string{"lane-group-0"}}},
		{"labels or", url.Values{"label": {"lane-group-0", "lane-group-1"}}, map[string]any{"labels": []string{"lane-group-0", "lane-group-1"}}},
		{"missing label", url.Values{"label": {"lane-missing"}}, map[string]any{"labels": []string{"lane-missing"}}},
		{"assignee", url.Values{"assignee": {"lane-owner-0"}}, map[string]any{"assignees": []string{"lane-owner-0"}}},
		{"label and assignee", url.Values{"label": {"lane-group-1"}, "assignee": {"lane-owner-0"}}, map[string]any{"labels": []string{"lane-group-1"}, "assignees": []string{"lane-owner-0"}}},
		{"type", url.Values{"type": {"bug"}}, map[string]any{"type": "bug"}},
		{"priorities", url.Values{"priority": {"0", "2"}}, map[string]any{"priorities": []int{0, 2}}},
		{"verification", url.Values{"verification": {"pending"}}, map[string]any{"verification": "pending"}},
		{"author", url.Values{"created_by": {"me"}}, map[string]any{"created_by": "me"}},
		{"open", url.Values{"open": {"true"}}, map[string]any{"open": true}},
		{"closed", url.Values{"open": {"false"}}, map[string]any{"open": false}},
		{"created after", url.Values{"created_after": {created}}, map[string]any{"created_after": created}},
		{"label state and type", url.Values{"label": {"lane-shared"}, "state": {runningState}, "type": {"bug"}}, map[string]any{"labels": []string{"lane-shared"}, "states": []string{runningState}, "type": "bug"}},
	} {
		t.Run("filtered "+test.name, func(t *testing.T) {
			want := listed(t, f, test.params)
			params := maps.Clone(test.params)
			params.Set("include", "work")
			params.Set("limit", "1")
			args := maps.Clone(test.args)
			args["project_id"], args["include"], args["limit"] = string(f.Project.ID), []string{"work"}, 1
			for pages := 0; ; pages++ {
				response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?"+params.Encode(), f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				hubfixture.Decode(t, response, &page)
				var tool operatortool.WorkReadResult[operatortool.NativeWorkPage]
				if err := json.Unmarshal(hubfixture.MCPData(t, call("tools/call", operatortool.WorkList, args), false), &tool); err != nil {
					t.Fatal(err)
				}
				if page.Work == nil || page.Work.Lanes == nil || tool.Data.Work == nil || tool.Data.Work.Lanes == nil {
					t.Fatalf("page %d: rest work present=%t, mcp work present=%t, want counted lanes on both", pages, page.Work != nil, tool.Data.Work != nil)
				}
				if got := lanes(page.Work.Lanes); !maps.Equal(got, want) {
					t.Fatalf("page %d: rest lanes=%v want listed=%v", pages, got, want)
				}
				if got := lanes(tool.Data.Work.Lanes); !maps.Equal(got, want) {
					t.Fatalf("page %d: mcp lanes=%v want listed=%v", pages, got, want)
				}
				if page.NextCursor == "" || pages > len(items) {
					break
				}
				params.Set("cursor", page.NextCursor)
				args["cursor"] = tool.Data.NextCursor
			}
		})
	}
	t.Run("tenants with identical labels", func(t *testing.T) {
		params := url.Values{"label": {"lane-shared"}, "include": {"work"}}
		for _, fixture := range []hubfixture.Native{f, other, twin} {
			want := listed(t, fixture, url.Values{"label": {"lane-shared"}})
			response := f.Hub.Request(t, http.MethodGet, fixture.Base+"/work-items?"+params.Encode(), fixture.Token, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page tracker.NativeIssuePage
			hubfixture.Decode(t, response, &page)
			if page.Work == nil {
				t.Fatalf("project %s returned no work summary", fixture.Project.ID)
			}
			if got := lanes(page.Work.Lanes); !maps.Equal(got, want) {
				t.Fatalf("project %s lanes=%v want listed=%v", fixture.Project.ID, got, want)
			}
		}
	})
	t.Run("search lanes stay unavailable", func(t *testing.T) {
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?include=work&q=lane", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page struct {
			Work struct {
				Lanes json.RawMessage `json:"lanes"`
			} `json:"work"`
		}
		hubfixture.Decode(t, response, &page)
		if string(page.Work.Lanes) != "null" {
			t.Fatalf("search lanes=%s want explicit unavailable null", page.Work.Lanes)
		}
	})
}

func createLaneItem(t *testing.T, fixture hubfixture.Native, title string, index int) tracker.NativeIssue {
	t.Helper()
	draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title + " " + testenv.DistinctText("lane-title-"+title, 6), Body: testenv.DistinctText("lane-"+title, 24) + "\n" + hubfixture.IssueBody(), State: "Todo",
		Labels: []string{"lane-shared", fmt.Sprintf("lane-group-%d", index%2)}, Assignees: []string{fmt.Sprintf("lane-owner-%d", index/2)}, Priority: new(index % 3)}
	if index%2 == 0 {
		draft.Type = "bug"
	}
	response := fixture.Request(t, http.MethodPost, fixture.Base+"/work-items", draft)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}

func TestWorkListLaneCountsFollowReporterVisibility(t *testing.T) {
	t.Parallel()
	hosted := hubfixture.NewHostedSecurity(t)
	project := hubtest.Project(t, hosted.Repository, hosted.Primary, hubtest.ProjectOptions{ID: "prj_reporter_lanes", Name: "Example reporter lanes", States: projectstore.TriageStates(hubfixture.States())})
	hubtest.Policy(t, hosted.Repository, hosted.Config.RunDetails, project, hubtest.PolicyOptions{Now: hosted.Now()})
	hosted.Project, hosted.Base = project.ID, hubfixture.ProjectBase(hosted.Primary, project.ID)
	writer := hosted.User(t, "writer", "member", "writer@example.test", "write", "")
	reporter := hosted.User(t, "reporter", "reporter", "reporter@example.test", "write", "")
	for index, author := range []hubfixture.HostedUser{writer, writer, writer, reporter, reporter} {
		state, body := "Todo", hubfixture.IssueBody()
		if author.Token == reporter.Token {
			state, body = "Triage", "## Steps to reproduce\nOpen the example page.\n## Expected\nThe page loads.\n## Actual\nThe page fails.\n## Where\nThe example settings page.\n## Environment\nExample browser."
		}
		body = testenv.DistinctText(fmt.Sprintf("reporter-lanes-%d", index), 24) + "\n" + body
		labels := []string{"lane-shared", fmt.Sprintf("lane-group-%d", index%2)}
		response := hosted.Request(t, author, http.MethodPost, hosted.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("reporter-lanes-%d", index)}, Title: []string{"Example kiwi orchard", "Example granite bridge", "Example violet harbor", "Example copper meadow", "Example silent glacier"}[index], Body: body, State: state, Type: "bug", Labels: labels})
		if response.Code != http.StatusOK {
			t.Fatalf("create %d status=%d body=%s", index, response.Code, response.Body.Bytes())
		}
		if index == 2 {
			var issue tracker.NativeIssue
			hubfixture.Decode(t, response, &issue)
			hubfixture.RequireStatus(t, hosted.Request(t, writer, http.MethodPost, hosted.Base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "reporter-lanes-done"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
		}
	}
	hosted.RefreshBoard(t, hosted.Primary)
	for _, test := range []struct {
		name   string
		viewer hubfixture.HostedUser
		filter url.Values
		total  int
	}{
		{"writer shared label", writer, url.Values{"label": {"lane-shared"}}, 5},
		{"writer label and open", writer, url.Values{"label": {"lane-group-0"}, "open": {"true"}}, 2},
		{"reporter shared label", reporter, url.Values{"label": {"lane-shared"}}, 2},
		{"reporter one label", reporter, url.Values{"label": {"lane-group-1"}}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, listed := map[string]tracker.NativeWorkLane{}, 0
			params := maps.Clone(test.filter)
			params.Set("limit", "1")
			for pages := 0; pages <= test.total; pages++ {
				response := hosted.Request(t, test.viewer, http.MethodGet, hosted.Base+"/work-items?"+params.Encode(), nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				hubfixture.Decode(t, response, &page)
				for _, issue := range page.Items {
					lane := want[issue.State]
					lane.State, lane.Total = issue.State, lane.Total+1
					want[issue.State] = lane
					listed++
				}
				if page.NextCursor == "" {
					break
				}
				params.Set("cursor", page.NextCursor)
			}
			if listed != test.total {
				t.Fatalf("listed=%d want fixture total %d", listed, test.total)
			}
			params = maps.Clone(test.filter)
			params.Set("include", "work")
			response := hosted.Request(t, test.viewer, http.MethodGet, hosted.Base+"/work-items?"+params.Encode(), nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page tracker.NativeIssuePage
			hubfixture.Decode(t, response, &page)
			if page.Work == nil || page.Work.Lanes == nil {
				t.Fatalf("work present=%t want counted lanes", page.Work != nil)
			}
			got := map[string]tracker.NativeWorkLane{}
			for _, lane := range page.Work.Lanes {
				got[lane.State] = lane
			}
			if !maps.Equal(got, want) {
				t.Fatalf("lanes=%v want listed=%v", got, want)
			}
		})
	}
}
