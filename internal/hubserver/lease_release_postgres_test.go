package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresLeaseReleaseOrdersLocksWithProgress(t *testing.T) {
	f := newRunnerPGFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	credential, status, err := f.service.authenticateAccessHash(ctx, apikey.HashToken(f.tokens[0]), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate: status=%d err=%v", status, err)
	}
	owner := nativeProgressOwner{service: f.service, scope: nativeScope{organization: "org_startup", project: "prj_runner", credential: credential}, request: progresscheck.Request{IssueID: string(f.leases[0].WorkItemID), PolicyID: f.policy.PolicyID, Instance: string(f.bindings[0].MachineID)}, leaseID: f.leases[0].ID, fencingToken: f.leases[0].FencingToken}
	locked, resume := make(chan struct{}), make(chan struct{})
	progress := make(chan error, 1)
	progressDone := make(chan struct{})
	t.Cleanup(func() { cancel(); close(resume); <-progressDone })
	go func() {
		defer close(progressDone)
		progress <- f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Background, func(w tenantstore.Writer) error {
			if _, err := w.RunnerIdentityByCredential(ctx, credential.ID); err != nil {
				return err
			}
			close(locked)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
			return owner.validateProgressLeaseWriter(ctx, w)
		})
	}()
	select {
	case <-locked:
	case err := <-progress:
		t.Fatalf("progress lock: %v", err)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		request := newHubAPIRequest(t, http.MethodPost, "/api/v2/organizations/org_startup/projects/prj_runner/leases/"+string(f.leases[0].ID)+"/release", f.tokens[0], tracker.NativeLeaseMutation{FencingToken: f.leases[0].FencingToken, Reason: "released"}).WithContext(ctx)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		result <- response
	}()
	testenv.WaitUntil(t, ctx, func() error {
		select {
		case response := <-result:
			t.Fatalf("release finished before lock wait: status=%d body=%s", response.Code, response.Body.String())
		default:
		}
		var waiting bool
		if err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%runner_identities%')`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("release has not reached runner identity lock")
		}
		return nil
	})
	resume <- struct{}{}
	if err := <-progress; err != nil {
		t.Errorf("progress deadlocked with release: %v", err)
	}
	requireNativeStatus(t, <-result, http.StatusNoContent)
}

type leaseReleaseErrorRepository struct {
	tenantstore.Repository
	failures chan error
}

func (r leaseReleaseErrorRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, run func(tenantstore.Writer) error) error {
	err := r.Repository.WriteTenant(ctx, organization, workload, run)
	if err != nil {
		r.failures <- err
	}
	return err
}

func TestPostgresLeaseReleaseRetriesAbortedTransaction(t *testing.T) {
	f := newRunnerPGFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocker, err := f.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `SET LOCAL deadlock_timeout='10s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `SELECT id FROM machines WHERE organization_id='org_startup' AND id=$1 FOR UPDATE`, string(f.bindings[0].MachineID)); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 1)
	f.service.config.TenantRepository = leaseReleaseErrorRepository{Repository: f.service.config.TenantRepository, failures: failures}
	path := "/api/v2/organizations/org_startup/projects/prj_runner/leases/" + string(f.leases[0].ID) + "/release"
	request := tracker.NativeLeaseMutation{FencingToken: f.leases[0].FencingToken, Reason: "cancelled", CancelAttempt: true}
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path, f.tokens[0], request).WithContext(ctx))
		result <- response
	}()
	testenv.WaitUntil(t, ctx, func() error {
		select {
		case response := <-result:
			t.Fatalf("release finished before lock wait: status=%d body=%s", response.Code, response.Body.String())
		default:
		}
		var waiting bool
		if err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) AND (query LIKE '%machines%' OR query LIKE '%MutateLeaseCommand%'))`, pid).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("release has not reached machine heartbeat lock")
		}
		return nil
	})
	if _, err := blocker.ExecContext(ctx, `SELECT id FROM issues WHERE organization_id='org_startup' AND native_id=$1 FOR UPDATE`, string(f.leases[0].WorkItemID)); err != nil {
		t.Fatalf("release was not the deadlock victim: %v", err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	response := <-result
	failure := <-failures
	var state interface{ SQLState() string }
	if !errors.As(failure, &state) || state.SQLState() != "40P01" {
		t.Fatalf("expected PostgreSQL deadlock, got %v", failure)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("aborted release status=%d body=%s", response.Code, response.Body.String())
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[0], request), http.StatusNoContent)
	if err := f.service.config.TenantRepository.ReadTenant(ctx, "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(ctx, string(f.leases[0].ID))
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(ctx, f.data[0].AttemptID)
		if err != nil {
			return err
		}
		if lease.ReleasedAt == nil || attempt.Status != "cancelled" || attempt.Sequence != 2 {
			return fmt.Errorf("retry lost atomic cancellation: lease=%+v attempt=%+v", lease, attempt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var finished int
	if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM collaboration_events WHERE organization_id='org_startup' AND attempt_id=$1 AND type='run.finished'`, f.data[0].AttemptID).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if finished != 1 {
		t.Fatalf("retried release committed %d terminal events", finished)
	}
}
