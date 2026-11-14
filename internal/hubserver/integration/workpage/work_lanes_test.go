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
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	items := make([]tracker.NativeIssue, 4)
	for i := range items {
		items[i] = f.Create(t, fmt.Sprintf("lane item %d", i))
	}
	leases := make([]tracker.NativeLease, 2)
	for i := range leases {
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, items[i], fmt.Sprintf("session-%d", i)))
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hubfixture.Decode(t, response, &leases[i])
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(items[i].WorkItemID)+"/events", r.Redemption.Credential, hubfixture.StartedEvent(leases[i])), http.StatusOK)
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
	f.Hub.RefreshBoard(t, f.Project.OrganizationID)
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
	t.Run("filter without exact summary", func(t *testing.T) {
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?include=work&label=missing-label&type=bug", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page struct {
			Work struct {
				Lanes json.RawMessage `json:"lanes"`
			} `json:"work"`
		}
		hubfixture.Decode(t, response, &page)
		if string(page.Work.Lanes) != "null" {
			t.Fatalf("unsupported filter lanes=%s want explicit unavailable null", page.Work.Lanes)
		}
	})
}
