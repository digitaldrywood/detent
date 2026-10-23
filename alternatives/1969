package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeLandingCapacityUnknown = -1

const nativeLandingCapacityProject = `CASE WHEN project.profile='native' AND lower(ws.detent_state)='merging'
 AND v.id IS NOT NULL AND COALESCE((SELECT json_extract(frozen.metadata_json,'$.gates.github_pull_request') FROM policy_revisions frozen WHERE frozen.scope=i.organization_id||'/'||i.project_id AND frozen.policy_id=v.policy_revision_id),json_extract(v.record_json,'$.policy.gates.github_pull_request'),0)=0 THEN i.project_id ELSE '' END`

const nativeGitHubLandingCapacityProject = `CASE WHEN project.profile='native' AND lower(ws.detent_state)='merging'
 AND v.id IS NOT NULL AND COALESCE((SELECT json_extract(frozen.metadata_json,'$.gates.github_pull_request') FROM policy_revisions frozen WHERE frozen.scope=i.organization_id||'/'||i.project_id AND frozen.policy_id=v.policy_revision_id),json_extract(v.record_json,'$.policy.gates.github_pull_request'),0)=1 THEN i.project_id ELSE '' END`

const nativePendingLandingClause = `c.work_item_id=i.native_id AND v.id IS NOT NULL
 AND COALESCE((SELECT json_extract(frozen.metadata_json,'$.gates.github_pull_request') FROM policy_revisions frozen WHERE frozen.scope=i.organization_id||'/'||i.project_id AND frozen.policy_id=v.policy_revision_id),json_extract(v.record_json,'$.policy.gates.github_pull_request'),0)=1
 AND COALESCE(json_extract(c.record_json,'$.landed.version_id'),'')<>v.id
 AND EXISTS (SELECT 1 FROM native_attempts pending
 WHERE pending.organization_id=i.organization_id AND pending.project_id=i.project_id AND pending.work_item_id=i.native_id
 AND pending.work_item_revision>=i.revision AND pending.dispatch_generation>=i.dispatch_generation
 AND pending.fencing_token=(SELECT max(latest.fencing_token) FROM native_attempts latest
 WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)
 AND COALESCE(json_extract(pending.data_json,'$.runtime.landing.waiting'),0)=1
 AND json_extract(pending.data_json,'$.runtime.landing.change_id')=c.id
 AND json_extract(pending.data_json,'$.runtime.landing.version_id')=v.id
 AND json_extract(pending.data_json,'$.runtime.landing.head_sha')=json_extract(v.record_json,'$.head_sha'))`

const nativeLandingCapacityJoins = `LEFT JOIN projects project ON project.id=i.project_id LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
 LEFT JOIN change_requests c ON c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.rowid=(SELECT max(latest.rowid) FROM change_requests latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)
 LEFT JOIN change_versions v ON v.id=json_extract(c.record_json,'$.current_version_id')`

func landingCapacityKey(machine tracker.MachineID, project, session string) string {
	parts := strings.Split(session, "/")
	if len(parts) != 3 || parts[0] != "landing" || parts[1] == "" || parts[2] == "" || project == "" {
		return ""
	}
	return string(machine) + "/" + project + "/" + parts[1]
}

func nativeLandingCapacityKey(ctx context.Context, query nativeQueryer, machine tracker.MachineID, session string, id tracker.WorkItemID) (string, error) {
	if landingCapacityKey(machine, "project", session) == "" {
		return "", nil
	}
	var project string
	err := query.QueryRowContext(ctx, `SELECT `+nativeLandingCapacityProject+` FROM issues i `+nativeLandingCapacityJoins+` WHERE i.id=? ORDER BY c.rowid DESC LIMIT 1`, id).Scan(&project)
	if err != nil {
		return "", err
	}
	return landingCapacityKey(machine, project, session), nil
}

func landingLeaseCapacityKey(machine tracker.MachineID, project, session, runner, lease string) string {
	if group := landingCapacityKey(machine, project, session); group != "" {
		return group + "/" + runner
	}
	return lease
}

func machineClaimUsage(ctx context.Context, query nativeQueryer, machine tracker.MachineID, runnerID string, now time.Time, joining string) (int, int, error) {
	rows, err := query.QueryContext(ctx, `SELECT l.expires_at, l.session_id, l.lease_id, COALESCE(lr.runner_id, ''), `+nativeLandingCapacityProject+`
 FROM leases l JOIN issues i ON i.id=l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id `+nativeLandingCapacityJoins+`
 WHERE l.machine_id=? AND l.released_at IS NULL`, machine)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	hostGroups, runnerGroups := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var expiry, session, lease, runner, project string
		if err := rows.Scan(&expiry, &session, &lease, &runner, &project); err != nil {
			return 0, 0, err
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return 0, 0, err
		}
		if !end.After(now) {
			continue
		}
		key := landingLeaseCapacityKey(machine, project, session, runner, lease)
		hostGroups[key] = true
		if runner == runnerID {
			runnerGroups[key] = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if joining != "" {
		delete(hostGroups, joining+"/"+runnerID)
		delete(runnerGroups, joining+"/"+runnerID)
	}
	return len(hostGroups), len(runnerGroups), nil
}

func nativeLandingClaimCapacity(ctx context.Context, query nativeQueryer, id tracker.WorkItemID, capacity int, now time.Time) (bool, int, error) {
	var project string
	if err := query.QueryRowContext(ctx, `SELECT CASE WHEN (`+nativePendingLandingClause+`) THEN i.project_id ELSE `+nativeGitHubLandingCapacityProject+` END FROM issues i `+nativeLandingCapacityJoins+` WHERE i.id=?`, id).Scan(&project); err != nil {
		return false, 0, err
	}
	if project == "" {
		return true, 0, nil
	}
	return nativeProjectLandingCapacity(ctx, query, project, capacity, "", now)
}

func nativeProjectLandingCapacity(ctx context.Context, query nativeQueryer, project string, capacity int, exclude tracker.LeaseID, now time.Time) (bool, int, error) {
	unknown := capacity == nativeLandingCapacityUnknown
	rows, err := query.QueryContext(ctx, `SELECT l.expires_at,l.landing_capacity FROM leases l JOIN issues i ON i.id=l.issue_id `+nativeLandingCapacityJoins+`
 WHERE i.project_id=? AND l.lease_id<>? AND l.released_at IS NULL AND l.expires_at>=? AND (l.landing_capacity>0 OR `+nativeGitHubLandingCapacityProject+`<>'')`, project, exclude, formatHubTime(now.Add(-time.Second)))
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	used := 0
	for rows.Next() {
		var expiry string
		var declared int
		if err := rows.Scan(&expiry, &declared); err != nil {
			return false, 0, err
		}
		expires, err := parseTimeValue(expiry)
		if err != nil {
			return false, 0, err
		}
		if !expires.After(now) {
			continue
		}
		used++
		if declared > 0 && (capacity <= 0 || declared < capacity) {
			capacity = declared
		}
	}
	if err := rows.Err(); err != nil {
		return false, 0, err
	}
	if unknown {
		if capacity > 0 && used >= capacity {
			return false, capacity, nil
		}
		if used > 0 {
			return false, nativeLandingCapacityUnknown, nil
		}
		return true, 0, nil
	}
	return capacity == 0 || used < capacity, capacity, nil
}

func reserveNativeLandingCapacity(ctx context.Context, tx *sql.Tx, scope nativeScope, record leaseRecord, capacity int, now time.Time) error {
	var pinned int
	if err := tx.QueryRowContext(ctx, "SELECT landing_capacity FROM leases WHERE lease_id=?", record.session.ID).Scan(&pinned); err != nil {
		return err
	}
	if pinned > 0 {
		return tightenNativeLandingCapacity(ctx, tx, record.session.ID, capacity)
	}
	var item string
	if err := tx.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id=?", record.issueID).Scan(&item); err != nil {
		return err
	}
	change, found, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	if !found || change.CurrentVersion == "" {
		return ErrNoClaimableWork
	}
	version, err := readChangeVersion(ctx, tx, change.ID, change.CurrentVersion)
	if err != nil {
		return err
	}
	if !version.Policy.Gates.GitHubPullRequest {
		return ErrNoClaimableWork
	}
	detail, err := readCurrentChangeDetail(ctx, tx, scope, change, now)
	if err != nil {
		return err
	}
	if !nativeChangeLandingReady("Merging", &detail) {
		return ErrNoClaimableWork
	}
	allowed, capacity, err := nativeProjectLandingCapacity(ctx, tx, string(scope.project), capacity, record.session.ID, now)
	if err != nil {
		return err
	}
	if !allowed {
		return &nativeError{Code: "runner_capacity", Message: "Project landing capacity is full", status: http.StatusConflict, publicMessage: true}
	}
	_, err = tx.ExecContext(ctx, "UPDATE leases SET landing_capacity=? WHERE lease_id=?", capacity, record.session.ID)
	return err
}

func tightenNativeLandingCapacity(ctx context.Context, tx *sql.Tx, lease tracker.LeaseID, capacity int) error {
	_, err := tx.ExecContext(ctx, "UPDATE leases SET landing_capacity=? WHERE lease_id=? AND landing_capacity>?", capacity, lease, capacity)
	return err
}

func requireLandingRenewalCapacity(ctx context.Context, query nativeQueryer, lease tracker.LeaseID, capacity int, now time.Time) error {
	var project string
	var pinned int
	if err := query.QueryRowContext(ctx, `SELECT CASE WHEN l.landing_capacity>0 THEN i.project_id ELSE `+nativeGitHubLandingCapacityProject+` END,l.landing_capacity
 FROM leases l JOIN issues i ON i.id=l.issue_id `+nativeLandingCapacityJoins+` WHERE l.lease_id=?`, lease).Scan(&project, &pinned); err != nil {
		return err
	}
	if project == "" {
		return nil
	}
	if capacity == 0 || pinned > 0 && pinned < capacity {
		capacity = pinned
	}
	allowed, _, err := nativeProjectLandingCapacity(ctx, query, project, capacity, lease, now)
	if err != nil {
		return err
	}
	if !allowed {
		return tracker.ErrLeaseConflict
	}
	return nil
}
