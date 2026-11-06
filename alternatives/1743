package hubserver

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

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
