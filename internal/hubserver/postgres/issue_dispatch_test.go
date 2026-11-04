package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssueDispatchPersistsAndFencesCurrentAttempt(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
		at := time.Now().UTC().Truncate(time.Microsecond)
		request := func(fence leasestore.Fence) error {
			return r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				dispatch, ok := w.(tenantstore.DispatchWriter)
				if !ok {
					t.Fatal("production PostgreSQL writer does not implement dispatch requests")
				}
				return dispatch.RequestIssueDispatch(t.Context(), "prj_1", "wi_1", fence, at)
			})
		}
		if err := request(leasestore.Fence{LeaseID: "unknown", FencingToken: 1}); !errors.Is(err, tracker.ErrStaleFencingToken) {
			t.Fatalf("fenced no-attempt request = %v", err)
		}
		if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(tenantstore.DispatchWriter).RequestIssueDispatch(t.Context(), "prj_2", "wi_1", leasestore.Fence{}, at)
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("wrong project request = %v", err)
		}
		if err := request(leasestore.Fence{}); err != nil {
			t.Fatal(err)
		}
		lease := claimLease(t, r, org, leaseClaim(1))
		event := attemptEvent(lease)
		event.DispatchGeneration = 1
		recordLeaseAttempt(t, r, org, event)
		if err := request(leaseFence(lease)); !errors.Is(err, tracker.ErrLeaseConflict) {
			t.Fatalf("live lease request = %v", err)
		}
		event.Sequence = 2
		event.Status = "succeeded"
		recordLeaseAttempt(t, r, org, event)
		if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			_, err := w.Release(t.Context(), leaseFence(lease), "completed")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := request(leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken + 1}); !errors.Is(err, tracker.ErrStaleFencingToken) {
			t.Fatalf("stale request = %v", err)
		}
		if err := request(leaseFence(lease)); err != nil {
			t.Fatal(err)
		}
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			state, err := reader.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), "prj_1", "wi_1")
			if err != nil {
				return err
			}
			if state.Generation != 2 || state.RequestedAt == nil || !state.RequestedAt.Equal(at) {
				t.Fatalf("persisted dispatch = %+v", state)
			}
			other, err := reader.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), "prj_2", "wi_2")
			if err == nil && other.Generation != 0 {
				t.Fatalf("other project dispatch = %+v", other)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(context.Background(), `UPDATE issues SET revision=revision+1 WHERE organization_id=$1 AND native_id='wi_1'`, org); err != nil {
			t.Fatal(err)
		}
		if err := request(leaseFence(lease)); err != nil {
			t.Fatalf("new source request = %v", err)
		}
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			state, err := reader.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), "prj_1", "wi_1")
			if err == nil && (state.Revision != 2 || state.Generation != 3) {
				t.Fatalf("new source dispatch = %+v", state)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		nextRequest := leaseClaim(1)
		nextRequest.ExpectedRevision = 2
		nextRequest.SessionID = "session_next"
		nextRequest.LeaseID = "lease_next"
		next := claimLease(t, r, org, nextRequest)
		nextEvent := attemptEvent(next)
		nextEvent.DispatchGeneration = 7
		recordLeaseAttempt(t, r, org, nextEvent)
		if err := request(leaseFence(lease)); !errors.Is(err, tracker.ErrStaleFencingToken) {
			t.Fatalf("superseded attempt request = %v", err)
		}
		nextEvent.Sequence = 2
		nextEvent.Status = "succeeded"
		recordLeaseAttempt(t, r, org, nextEvent)
		if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			_, err := w.Release(t.Context(), leaseFence(next), "completed")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var before leasestore.Attempt
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			var err error
			before, err = reader.Attempt(t.Context(), nextEvent.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET dispatch_generation=0,dispatch_requested_at=NULL WHERE organization_id=$1 AND project_id='prj_1' AND native_id='wi_1'`, org); err != nil {
			t.Fatal(err)
		}
		if err := request(leaseFence(next)); err != nil {
			t.Fatal(err)
		}
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			state, err := reader.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), "prj_1", "wi_1")
			if err == nil && (state.Generation != 8 || state.Revision != 2 || state.RequestedAt == nil || !state.RequestedAt.Equal(at)) {
				t.Fatalf("request behind latest attempt = %+v", state)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			after, err := reader.Attempt(t.Context(), nextEvent.ID)
			if err == nil && !reflect.DeepEqual(before, after) {
				t.Fatalf("dispatch rewrote attempt: before=%+v after=%+v", before, after)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := request(leasestore.Fence{}); !errors.Is(err, tracker.ErrStaleFencingToken) {
			t.Fatalf("unfenced existing attempt request = %v", err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET archived=true WHERE organization_id=$1 AND project_id='prj_1' AND native_id='wi_1'`, org); err != nil {
			t.Fatal(err)
		}
		if err := request(leaseFence(next)); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("archived request = %v", err)
		}
	}
}

func TestIssueDispatchRollbackRefusesAcrossTenantRLS(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	at := time.Now().UTC().Truncate(time.Microsecond)
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, org := range organizations {
		seedLeaseFamily(t, r, org)
		if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(tenantstore.DispatchWriter).RequestIssueDispatch(t.Context(), "prj_1", "wi_1", leasestore.Fence{}, at)
		}); err != nil {
			t.Fatal(err)
		}
	}
	admin.SetMaxIdleConns(1)
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
			t.Error(err)
		}
	}()
	var hidden int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issues`).Scan(&hidden); err != nil || hidden != 0 {
		t.Fatalf("unscoped migration role did not exercise tenant RLS: count=%d error=%v", hidden, err)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	before, err := provider.GetDBVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(t.Context()); err == nil || !strings.Contains(err.Error(), "dispatch requests require an explicit retained-data migration") {
		t.Fatalf("rollback returned %v, want retained-data refusal", err)
	}
	after, err := provider.GetDBVersion(t.Context())
	if err != nil || after != before {
		t.Fatalf("rollback schema version=%d, want %d: %v", after, before, err)
	}
	for _, org := range organizations {
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			state, err := reader.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), "prj_1", "wi_1")
			if err == nil && (state.Generation != 1 || state.RequestedAt == nil || !state.RequestedAt.Equal(at)) {
				t.Errorf("rollback changed retained request for %s: %+v", org, state)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
