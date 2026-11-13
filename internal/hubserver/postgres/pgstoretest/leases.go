package pgstoretest

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func SeedLeaseFamily(t *testing.T, r *postgres.Repository, org tracker.OrganizationID) {
	t.Helper()
	err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		if err := SeedCoreOrganization(t.Context(), w, org); err != nil {
			return err
		}
		for id := int64(1); id <= 2; id++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: id, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", id)), Number: 1, Title: "Example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		for _, id := range []string{"machine_1", "machine_2"} {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func LeaseClaim(id int64) leasestore.Claim {
	return leasestore.Claim{IssueID: id, ExpectedRevision: 1, MachineID: fmt.Sprintf("machine_%d", id), SessionID: fmt.Sprintf("session_%d", id), LeaseID: fmt.Sprintf("lease_%d", id), TTL: time.Hour}
}

func ClaimLease(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, request leasestore.Claim) leasestore.Lease {
	t.Helper()
	var lease leasestore.Lease
	err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		var err error
		lease, err = w.Claim(t.Context(), request)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func LeaseFence(lease leasestore.Lease) leasestore.Fence {
	return leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
}

func AttemptEvent(lease leasestore.Lease) leasestore.AttemptEvent {
	return leasestore.AttemptEvent{ID: "attempt_" + lease.LeaseID, Fence: LeaseFence(lease), RunID: "run_" + lease.LeaseID, Sequence: 1, Status: "running", PayloadObjectKey: "example/start", DispatchGeneration: 2}
}

func RecordLeaseAttempt(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, event leasestore.AttemptEvent) leasestore.Attempt {
	t.Helper()
	var attempt leasestore.Attempt
	err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), event)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func WaitLeaseLock(t *testing.T, admin *sql.DB, count int, tasks ...*RepositoryTask) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		for _, task := range tasks {
			select {
			case <-task.Done:
				t.Fatalf("transaction exited before the lock barrier: %v", task.Err)
			default:
			}
		}
		var waiting int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting < count {
			return fmt.Errorf("waiting transactions = %d, want %d", waiting, count)
		}
		return nil
	})
}
