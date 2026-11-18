package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerSyncChanges(t *testing.T) {
	t.Parallel()
	f := newRunnerPGFixture(t)
	binding := f.bindings[0]
	path := "/api/v2/organizations/" + string(f.organization) + "/runners/" + binding.RunnerID + "/sync"
	for _, id := range []tracker.ProjectID{"prj_two", "prj_three"} {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.organization, ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.organization, TokenID: "credential_1", ProjectID: id})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var versions json.RawMessage
	for _, step := range []string{"initial", "idle", "routing change", "idle again"} {
		if step == "routing change" {
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
				r, err := w.RunnerIdentity(t.Context(), binding.RunnerID)
				if err != nil {
					return err
				}
				r.DisplayName = "Updated"
				return w.UpdateRunnerRouting(t.Context(), r.Revision, r)
			}); err != nil {
				t.Fatal(err)
			}
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[0], map[string]any{"machine_id": binding.MachineID, "versions": versions})
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			Versions json.RawMessage `json:"versions"`
			Routing  *struct {
				Routing struct {
					DisplayName string `json:"display_name"`
				} `json:"routing"`
			} `json:"routing"`
			Projects map[string]json.RawMessage `json:"projects"`
			NoChange bool                       `json:"no_change"`
		}
		decodeHubResponse(t, response, &result)
		switch step {
		case "initial":
			if len(result.Projects) != 3 || result.Routing == nil || len(result.Versions) == 0 || result.NoChange {
				t.Fatalf("incomplete initial sync: %s", response.Body)
			}
		case "routing change":
			if result.NoChange || result.Routing == nil || result.Routing.Routing.DisplayName != "Updated" || len(result.Projects) != 0 {
				t.Fatalf("incorrect routing delta: %s", response.Body)
			}
		default:
			if !result.NoChange || result.Routing != nil || len(result.Projects) != 0 {
				t.Fatalf("unchanged sync returned deltas: %s", response.Body)
			}
		}
		if result.Versions != nil {
			versions = result.Versions
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], map[string]any{"machine_id": binding.MachineID}), http.StatusNotFound)
}

func TestRunnerSyncPushClaims(t *testing.T) {
	t.Parallel()
	f, probe := newDispatchWaitFixture(t, "sync claims")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	ctx, cancel := context.WithCancel(t.Context())
	var joined sync.WaitGroup
	defer func() { cancel(); joined.Wait() }()
	publish, items := prepareDispatchIssuesFixture(t, f, []tracker.CreateIssue{
		{Title: "First", Body: issueContractTestSections, State: "Todo"},
		{Title: "Second", Body: issueContractTestSections, State: "Todo"},
	})
	workers := []runnerFixture{prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat), prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)}
	for i := range workers {
		workers[i].redemption.Capacity = 1
		workers[i].enroll(t)
		workers[i].reportCapacity(t)
	}
	responses := make(chan *httptest.ResponseRecorder, len(workers))
	for index, r := range workers {
		request := runnerauth.SyncRequest{MachineID: r.binding.MachineID}
		path := r.identityPath() + "/sync"
		initial := performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, request)
		requireNativeStatus(t, initial, http.StatusOK)
		var state runnerauth.SyncResponse
		decodeHubResponse(t, initial, &state)
		request.Versions = *state.Versions
		request.WaitSeconds = 30
		request.Claim = &runnerauth.SyncClaim{SessionID: fmt.Sprintf("sync-%d", index), TTLSeconds: 90, Projects: map[tracker.ProjectID]tracker.NativeClaim{f.project.ID: {PolicyID: hubTestPolicy().ID, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}}}}
		req := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, request).WithContext(ctx)
		joined.Go(func() {
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, req)
			responses <- response
		})
	}
	awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, len(workers), probe)
	publish()
	leases := map[tracker.NativeWorkItemID]tracker.NativeLease{}
	for range workers {
		response := <-responses
		requireNativeStatus(t, response, http.StatusOK)
		var result runnerauth.SyncResponse
		decodeHubResponse(t, response, &result)
		if result.Lease == nil || result.LeaseProject != f.project.ID || result.Lease.FencingToken <= 0 {
			t.Fatalf("missing pushed lease: %s", response.Body)
		}
		leases[result.Lease.WorkItemID] = *result.Lease
	}
	a, b := leases[items[0].WorkItemID], leases[items[1].WorkItemID]
	if a.ID == "" || b.ID == "" || a.MachineID == b.MachineID || a.FencingToken == b.FencingToken {
		t.Fatalf("invalid assignments: %+v", leases)
	}
}
