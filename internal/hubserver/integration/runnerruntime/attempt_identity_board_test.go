package runnerruntime_test

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptIdentityReadCounter struct {
	attachment.DetailStore
	mu   sync.Mutex
	keys []string
}

func (d *attemptIdentityReadCounter) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	d.mu.Lock()
	d.keys = append(d.keys, ref.Key)
	d.mu.Unlock()
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func (d *attemptIdentityReadCounter) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := d.keys
	d.keys = nil
	return keys
}

func TestPostgresBoardCardsKeepRecordedAttemptIdentity(t *testing.T) {
	counter := &attemptIdentityReadCounter{}
	f := hubfixture.NewLeasedRunnersWithDetails(t, func(details attachment.DetailStore) attachment.DetailStore {
		counter.DetailStore = details
		return counter
	})
	base := "/api/v2/organizations/" + string(f.Organization) + "/projects/prj_runner"
	reader := f.Tokens[0]
	owners := map[tracker.NativeWorkItemID]string{f.Leases[0].WorkItemID: f.Tokens[0], f.Leases[1].WorkItemID: f.Tokens[1]}
	observe := func(t *testing.T, index int, data tracker.NativeRunData, key, model, effort string) tracker.NativeRunData {
		t.Helper()
		native, ctx := f.Client(t, index)
		data.Sequence++
		now := time.Now().UTC()
		data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now, Identity: agentidentity.RuntimeUpdate(model, "", effort, "", now)}
		if err := native.AppendEvent(ctx, f.Leases[index].WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: key}, Type: "run.observed", SchemaVersion: 1, Data: data}); err != nil {
			t.Fatal(err)
		}
		return data
	}
	card := func(t *testing.T, item tracker.NativeWorkItemID) tracker.NativeBoardCard {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, base+"/work-items?include=board&limit=50", reader, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		hubfixture.Decode(t, response, &page)
		for _, card := range page.Cards {
			if card.Issue.WorkItemID == item {
				return card
			}
		}
		t.Fatalf("board has no card for %s", item)
		return tracker.NativeBoardCard{}
	}
	latestAttempt := func(t *testing.T, item tracker.NativeWorkItemID) tracker.NativeAttempt {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, base+"/work-items/"+string(item)+"/attempts?direction=newest", owners[item], nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var attempts tracker.Page[tracker.NativeAttempt]
		hubfixture.Decode(t, response, &attempts)
		if len(attempts.Items) == 0 {
			t.Fatalf("no attempts for %s", item)
		}
		return attempts.Items[0]
	}
	rebuild := func(t *testing.T, items ...tracker.NativeWorkItemID) {
		t.Helper()
		for _, item := range items {
			if _, err := f.Admin.ExecContext(t.Context(), "INSERT INTO native_board_dirty(organization_id,project_id,work_item_id) VALUES($1,'prj_runner',$2) ON CONFLICT DO NOTHING", f.Organization, item); err != nil {
				t.Fatal(err)
			}
		}
		f.Hub.RefreshBoard(t, f.Organization)
	}
	type expectation struct {
		item     tracker.NativeWorkItemID
		status   string
		running  bool
		identity tracker.NativeExecutionIdentity
	}
	requireCards := func(t *testing.T, label string, want []expectation) {
		t.Helper()
		for _, want := range want {
			got := card(t, want.item)
			if got.Attempt == nil || got.Attempt.Status != want.status || got.Attempt.Identity == nil || *got.Attempt.Identity != want.identity {
				t.Fatalf("%s %s attempt=%+v identity=%+v, want status %s identity %+v", label, want.item, got.Attempt, identityValue(got.Attempt), want.status, want.identity)
			}
			if running := got.Execution != nil && got.Execution.Running; running != want.running {
				t.Fatalf("%s %s execution=%+v, want running=%t", label, want.item, got.Execution, want.running)
			}
			latest := latestAttempt(t, want.item)
			if latest.AttemptID != got.Attempt.ID || latest.Identity == nil || *latest.Identity != want.identity {
				t.Fatalf("%s %s attempts API identity=%+v attempt=%s, board attempt=%s identity=%+v", label, want.item, latest.Identity, latest.AttemptID, got.Attempt.ID, want.identity)
			}
		}
	}
	released, active := f.Leases[0].WorkItemID, f.Leases[1].WorkItemID
	observe(t, 0, f.Data[0], "released-observed", "resolved-model", "high")
	observe(t, 1, f.Data[1], "active-observed", "active-model", "low")
	native, ctx := f.Client(t, 0)
	if err := native.Release(ctx, f.Leases[0], "released"); err != nil {
		t.Fatal(err)
	}
	f.Hub.RefreshBoard(t, f.Organization)
	want := []expectation{
		{released, "cancelled", false, tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "resolved-model", ReasoningEffort: "high"}},
		{active, "running", true, tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "active-model", ReasoningEffort: "low"}},
	}
	requireCards(t, "recorded", want)

	changed := hubfixture.Policy()
	changed.Gates.SecurityAudit = true
	approved, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareProjectPolicy(t.Context(), f.Organization, changed.WithID())
	if err != nil {
		t.Fatal(err)
	}
	approved.ProjectID, approved.ApprovedAt, approved.ApprovedBy = "prj_runner", time.Now().UTC().Truncate(time.Microsecond), "test_hub_admin"
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), approved, f.Policy.PolicyID)
	}); err != nil {
		t.Fatal(err)
	}
	rebuild(t, released, active)
	requireCards(t, "after policy change", want)

	attemptKeys := func(t *testing.T) map[string]bool {
		t.Helper()
		rows, err := f.Admin.QueryContext(t.Context(), "SELECT a.payload_object_key FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.native_id=$2", f.Organization, released)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		keys := map[string]bool{}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				t.Fatal(err)
			}
			keys[key] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return keys
	}
	requireRefreshSkipsRetainedPayloads := func(t *testing.T, attempts int) {
		t.Helper()
		if keys := attemptKeys(t); len(keys) != attempts {
			t.Fatalf("history has %d attempts, want %d", len(keys), attempts)
		}
		counter.take()
		rebuild(t, released)
		reads := counter.take()
		payloads := attemptKeys(t)
		for _, key := range reads {
			if payloads[key] {
				t.Fatalf("board refresh read attempt payload %s with %d retained attempts", key, attempts)
			}
		}
	}
	requireRefreshSkipsRetainedPayloads(t, 1)
	for index := range 2 {
		model := "history-model-" + strconv.Itoa(index)
		lease, data := claimHistoricalAttempt(t, f, released, index)
		native, err := api.New(api.Config{URL: f.Server.URL, TokenSource: func() string { return f.Tokens[0] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
		if err != nil {
			t.Fatal(err)
		}
		client, err := native.Native(f.Organization, "prj_runner")
		if err != nil {
			t.Fatal(err)
		}
		ctx := api.WithMutationAuthority(t.Context(), client.Scope(), lease, nil)
		if err := client.AppendEvent(ctx, released, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-start-" + strconv.Itoa(index)}, Type: "run.started", SchemaVersion: 1, Data: data}); err != nil {
			t.Fatal(err)
		}
		data.Sequence++
		now := time.Now().UTC()
		data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now, Identity: agentidentity.RuntimeUpdate(model, "", "medium", "", now)}
		if err := client.AppendEvent(ctx, released, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-observed-" + strconv.Itoa(index)}, Type: "run.observed", SchemaVersion: 1, Data: data}); err != nil {
			t.Fatal(err)
		}
		if err := client.Release(ctx, lease, "released"); err != nil {
			t.Fatal(err)
		}
		want[0].identity.Model, want[0].identity.ReasoningEffort = model, "medium"
	}
	requireRefreshSkipsRetainedPayloads(t, 3)
	requireCards(t, "latest of retained history", want)
}

func identityValue(attempt *tracker.NativeBoardAttempt) any {
	if attempt == nil || attempt.Identity == nil {
		return nil
	}
	return *attempt.Identity
}

func claimHistoricalAttempt(t *testing.T, f hubfixture.LeasedRunners, item tracker.NativeWorkItemID, index int) (tracker.NativeLease, tracker.NativeRunData) {
	t.Helper()
	now := time.Now().UTC()
	var lease tracker.NativeLease
	err := f.Hub.Repository.WriteTenant(t.Context(), f.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.WorkItem(t.Context(), "prj_runner", item)
		if err != nil {
			return err
		}
		suffix := strconv.Itoa(index)
		claimed, err := w.Claim(t.Context(), leasestore.Claim{IssueID: current.ID, ExpectedRevision: current.Revision, MachineID: string(f.Bindings[0].MachineID), SessionID: "history_" + suffix, LeaseID: "lease_history_" + suffix, TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: f.Organization, LeaseID: claimed.LeaseID, RunnerID: f.Bindings[0].RunnerID, AssignedAt: now}); err != nil {
			return err
		}
		requirements := hubfixture.Policy().Requirements
		if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: f.Organization, ProjectID: "prj_runner", LeaseID: tracker.LeaseID(claimed.LeaseID), PolicyID: f.Policy.PolicyID, BoundAt: now, Selector: &requirements}); err != nil {
			return err
		}
		lease = tracker.NativeLease{ID: tracker.LeaseID(claimed.LeaseID), WorkItemID: item, MachineID: f.Bindings[0].MachineID, SessionID: claimed.SessionID, FencingToken: tracker.FencingToken(claimed.FencingToken), PolicyID: f.Policy.PolicyID, ExpiresAt: claimed.ExpiresAt}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: f.Policy.PolicyID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
	return lease, data
}
