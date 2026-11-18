package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type syncTraceKey struct{}

type syncWriteTrace struct {
	dispatchWaitPostgresTrace
	writes atomic.Int64
}

func (p *syncWriteTrace) observe(tag pgconn.CommandTag) {
	if tag.Insert() || tag.Update() || tag.Delete() {
		p.writes.Add(tag.RowsAffected())
	}
}
func (p *syncWriteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(syncTraceKey{}) == p {
		p.observe(data.CommandTag)
	}
}
func (p *syncWriteTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if ctx.Value(syncTraceKey{}) == p {
		p.observe(data.CommandTag)
	}
}

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
	probe := new(syncWriteTrace)
	installRunnerTestPool(t, f, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, probe)
	})
	report := runnerauth.MachineReport{DisplayName: "Example", Capacity: 1, Version: "1.0.0", OS: "linux", Architecture: "amd64", ProtocolMajor: 2}
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
		before := probe.writes.Load()
		request := newHubAPIRequest(t, http.MethodPost, path, f.tokens[0], map[string]any{"machine_id": binding.MachineID, "report": report, "versions": versions})
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(t.Context(), syncTraceKey{}, probe)))
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
			if probe.writes.Load() == before {
				t.Fatal("initial report did not write machine state")
			}
			if len(result.Projects) != 3 || result.Routing == nil || len(result.Versions) == 0 || result.NoChange {
				t.Fatalf("incomplete initial sync: %s", response.Body)
			}
		case "routing change":
			if result.NoChange || result.Routing == nil || result.Routing.Routing.DisplayName != "Updated" || len(result.Projects) != 0 {
				t.Fatalf("incorrect routing delta: %s", response.Body)
			}
		default:
			if probe.writes.Load() != before {
				t.Fatalf("%s wrote %d rows", step, probe.writes.Load()-before)
			}
			if !result.NoChange || result.Routing != nil || len(result.Projects) != 0 {
				t.Fatalf("unchanged sync returned deltas: %s", response.Body)
			}
		}
		if result.Versions != nil {
			versions = result.Versions
		}
	}
	held := runnerauth.HeldLease{Project: "prj_runner", Lease: f.leases[0].ID, FencingToken: f.leases[0].FencingToken, TTLSeconds: 90}
	req := runnerauth.SyncRequest{MachineID: binding.MachineID, Held: []runnerauth.HeldLease{held}}
	renewed := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[0], req)
	requireNativeStatus(t, renewed, http.StatusOK)
	var granted runnerauth.SyncResponse
	decodeHubResponse(t, renewed, &granted)
	if len(granted.Renewed) != 1 || granted.Renewed[0].ID != held.Lease || granted.Renewed[0].FencingToken != held.FencingToken || !granted.Renewed[0].ExpiresAt.After(granted.Renewed[0].RenewedAt) {
		t.Fatalf("invalid renewal: %s", renewed.Body)
	}
	req.Held[0].FencingToken++
	stale := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[0], req)
	requireNativeStatus(t, stale, http.StatusConflict)
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
		{Title: "Unavailable", Body: issueContractTestSections, State: "Todo", Priority: new(1)},
		{Title: "First", Body: issueContractTestSections, State: "Todo"},
		{Title: "Second", Body: issueContractTestSections, State: "Todo"},
	})
	raw, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	var missing string
	probe.details.mu.Lock()
	for key, data := range probe.details.data {
		if bytes.Equal(data, raw) {
			missing = key
			delete(probe.details.data, key)
			break
		}
	}
	probe.details.keyReads = make(map[string]int)
	probe.details.mu.Unlock()
	if missing == "" {
		t.Fatal("missing fixture did not match the stored first candidate")
	}
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
	probe.details.mu.Lock()
	attempted := probe.details.keyReads[missing]
	probe.details.mu.Unlock()
	if attempted == 0 {
		t.Fatal("unreadable first candidate was never considered")
	}
	a, b := leases[items[1].WorkItemID], leases[items[2].WorkItemID]
	if a.ID == "" || b.ID == "" || a.MachineID == b.MachineID || a.FencingToken == b.FencingToken {
		t.Fatalf("invalid assignments: %+v", leases)
	}
}
