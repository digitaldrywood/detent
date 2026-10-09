package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeSourceCheckpoint struct {
	AttemptID                    string
	MachineID                    tracker.MachineID
	RunnerID, RunnerName, Status string
	Released                     sql.NullString
	Checkpoint                   *tracker.NativeCheckpoint
}

func readNativeSourceCheckpoint(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (nativeSourceCheckpoint, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,l.machine_id,COALESCE(lr.runner_id,''),COALESCE(NULLIF(r.display_name,''),m.hostname,''),a.status,l.released_at,a.checkpoint_json
FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=a.organization_id LEFT JOIN machines m ON m.id=l.machine_id AND m.organization_id=a.organization_id
WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND a.checkpoint_json IS NOT NULL ORDER BY a.fencing_token DESC`, scope.organization, scope.project, item)
	if err != nil {
		return nativeSourceCheckpoint{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var source nativeSourceCheckpoint
		var raw string
		if err := rows.Scan(&source.AttemptID, &source.MachineID, &source.RunnerID, &source.RunnerName, &source.Status, &source.Released, &raw); err != nil {
			return source, err
		}
		if err := json.Unmarshal([]byte(raw), &source.Checkpoint); err != nil {
			return source, err
		}
		if source.Checkpoint.PreservesSource() {
			return source, nil
		}
	}
	return nativeSourceCheckpoint{}, rows.Err()
}

// nativeSourceClaimAllowed never routes work by where earlier work ran
// (INV-17). It refuses only a Hub-recorded source that no runner can restore.
func nativeSourceClaimAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID) (bool, string, error) {
	var item string
	if err := q.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id=? AND organization_id=? AND project_id=?", id, scope.organization, scope.project).Scan(&item); err != nil {
		return false, "", err
	}
	source, err := readNativeSourceCheckpoint(ctx, q, scope, item)
	if err != nil {
		return false, "", err
	}
	checkpoint := source.Checkpoint
	owned := func(owner tracker.MachineID, ownerRunner string) bool {
		return owner != "" && machine == owner && (ownerRunner == "" || ownerRunner == scope.credential.Runner.RunnerID)
	}
	if checkpoint.UncertainForgeEffect() {
		return owned(source.MachineID, source.RunnerID), fmt.Sprintf("Checkpoint %s on source runner %s has an uncertain Git push or PR creation; that runner reconciles it first", source.AttemptID, source.MachineID), nil
	}
	if checkpoint.GitRef() {
		return true, "Checkpoint " + checkpoint.Ref + " is in the project repository; any eligible runner restores it", nil
	}
	change, found, err := readLatestNativeChangeRequest(ctx, q, scope, item)
	if err != nil {
		return false, "", err
	}
	var version tracker.ChangeVersion
	if found && change.CurrentVersion != "" {
		version, err = readChangeVersion(ctx, q, change.ID, change.CurrentVersion)
		if err != nil {
			return false, "", err
		}
		stored, err := nativeChangeSourceStored(ctx, q, version)
		if err != nil || stored {
			return stored, "Verified Change source is stored on the Hub; any eligible runner restores it", err
		}
	}
	if checkpoint != nil && (checkpoint.Availability == "missing" || checkpoint.Availability == "inaccessible" || checkpoint.Storage == "customer_store") {
		return false, fmt.Sprintf("Checkpoint %s on source runner %s is %s; restore and verify source before recovery", source.AttemptID, source.MachineID, checkpoint.Availability), nil
	}
	if version.ID != "" {
		owner, ownerRunner := source.MachineID, source.RunnerID
		if checkpoint == nil {
			err = q.QueryRowContext(ctx, `SELECT l.machine_id,COALESCE(lr.runner_id,'') FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
WHERE a.id=? AND a.organization_id=? AND a.project_id=? AND a.work_item_id=?`, version.AttemptID, scope.organization, scope.project, item).Scan(&owner, &ownerRunner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, "", err
			}
		}
		reason := fmt.Sprintf("Change %s version %s has no verified Hub source; recapture the exact head on source runner %s", change.ID, version.ID, owner)
		return owned(owner, ownerRunner), reason, nil
	}
	return true, "No source is pinned to a runner", nil
}

func nativeChangeSourceStored(ctx context.Context, q nativeQueryer, version tracker.ChangeVersion) (bool, error) {
	if version.Source == nil {
		return false, nil
	}
	var bundle []byte
	err := q.QueryRowContext(ctx, "SELECT bundle FROM change_sources WHERE version_id=?", version.ID).Scan(&bundle)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle) == nil, nil
}
