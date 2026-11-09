package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedHealthAttemptRecord(t *testing.T, f nativeFixture, item tracker.NativeIssue, machine, status string, data tracker.NativeRunData, at time.Time, cost float64) string {
	t.Helper()
	organization := f.project.OrganizationID
	var lease leasestore.Lease
	var runnerID string
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.Machine(t.Context(), machine); errors.Is(err, corestore.ErrNotFound) {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machine, Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		row, err := w.WorkItem(t.Context(), f.project.ID, item.WorkItemID)
		if err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: machine, SessionID: newNativeID("session"), LeaseID: newNativeID("lease"), TTL: time.Hour})
		if err != nil {
			return err
		}
		runners, err := w.RunnerIdentityByMachine(t.Context(), tracker.MachineID(machine), 200)
		if err != nil {
			return err
		}
		if len(runners) == 1 {
			runnerID = runners[0].RunnerID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data.AttemptID, data.RunID, data.LeaseID, data.FencingToken, data.Sequence, data.Outcome = newNativeID("attempt"), newNativeID("run"), tracker.LeaseID(lease.LeaseID), tracker.FencingToken(lease.FencingToken), 1, status
	if data.SessionID == "" {
		data.SessionID = lease.SessionID
	}
	if data.RunnerID == "" {
		data.RunnerID = runnerID
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(organization), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: fence, RunID: data.RunID, Sequence: 1, Status: status, StartedAt: at.Add(-time.Minute), PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
			return err
		}
		if runnerID != "" {
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: f.service.config.now()}); err != nil {
				return err
			}
		}
		if status != "running" {
			_, err := w.Release(t.Context(), fence, "fixture_history")
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEvent(t, f, item, "run.finished", tracker.CollaborationData{Run: &data}, at)
	admin := healthFixtureDatabase(t, f)
	if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET started_at=$1,updated_at=$2 WHERE organization_id=$3 AND id=$4`, at.Add(-time.Minute), at, organization, data.AttemptID); err != nil {
		t.Fatal(err)
	}
	var released any
	if status != "running" {
		released = at
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=$1,renewed_at=$2,expires_at=$3,released_at=$4 WHERE organization_id=$5 AND lease_id=$6`, at.Add(-time.Minute), at, at.Add(time.Minute), released, organization, lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	if cost >= 0 {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'example','example',0,0,0,$5,'USD',$6)`, organization, f.project.ID, data.AttemptID, at.Truncate(time.Hour), cost, at); err != nil {
			t.Fatal(err)
		}
	}
	return data.AttemptID
}

func seedHealthRateAttempt(t *testing.T, tx *sql.Tx, scope nativeScope, item tracker.NativeIssue, machine string, status string, data tracker.NativeRunData, at time.Time, cost float64) string {
	t.Helper()
	leaseID, attemptID := newNativeID("lease"), newNativeID("attempt")
	var itemID int64
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", item.WorkItemID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	result, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, leaseID, itemID, machine, leaseID, formatHubTime(at.Add(time.Minute)), formatHubTime(at), formatHubTime(at), formatHubTime(at), formatHubTime(at), formatHubTime(at))
	if err != nil {
		t.Fatal(err)
	}
	fence, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?,?,?)`, attemptID, scope.organization, scope.project, item.WorkItemID, leaseID, fence, attemptID, status, string(raw), formatHubTime(at.Add(-time.Minute)), formatHubTime(at)); err != nil {
		t.Fatal(err)
	}
	if cost >= 0 {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,cost_estimate,updated_at) VALUES(?,?,?,?,?,?,?,?)`, attemptID, scope.organization, scope.project, formatHubTime(at.Truncate(time.Hour)), "codex", "test", cost, formatHubTime(at)); err != nil {
			t.Fatal(err)
		}
	}
	return attemptID
}
