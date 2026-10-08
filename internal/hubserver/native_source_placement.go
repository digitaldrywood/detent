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

func nativeSourceClaimAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID) (bool, string, error) {
	var item string
	if err := q.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id=? AND organization_id=? AND project_id=?", id, scope.organization, scope.project).Scan(&item); err != nil {
		return false, "", err
	}
	source, err := readNativeSourceCheckpoint(ctx, q, scope, item)
	if err != nil {
		return false, "", err
	}
	owner, ownerRunner, attempt, checkpoint := source.MachineID, source.RunnerID, source.AttemptID, source.Checkpoint

	change, found, err := readLatestNativeChangeRequest(ctx, q, scope, item)
	if err != nil {
		return false, "", err
	}
	if found && change.CurrentVersion != "" {
		version, err := readChangeVersion(ctx, q, change.ID, change.CurrentVersion)
		if err != nil {
			return false, "", err
		}
		var sourceOwner tracker.MachineID
		var sourceRunner string
		err = q.QueryRowContext(ctx, `SELECT l.machine_id,COALESCE(lr.runner_id,'') FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
WHERE a.id=? AND a.organization_id=? AND a.project_id=? AND a.work_item_id=?`, version.AttemptID, scope.organization, scope.project, item).Scan(&sourceOwner, &sourceRunner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, "", err
		}
		matches := checkpoint == nil || checkpoint.WorktreeState != "dirty" && checkpoint.WorktreeState != "unknown" && checkpoint.HeadSHA == version.HeadSHA
		if !matches && checkpoint != nil && (checkpoint.WorktreeState == "clean" || checkpoint.WorktreeState == "unpushed") && version.Source != nil {
			var priorRaw string
			err := q.QueryRowContext(ctx, `SELECT record_json FROM change_versions WHERE change_id=? AND json_extract(record_json,'$.head_sha')=? ORDER BY rowid DESC LIMIT 1`, change.ID, checkpoint.HeadSHA).Scan(&priorRaw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, "", err
			}
			if err == nil {
				var prior tracker.ChangeVersion
				if err := json.Unmarshal([]byte(priorRaw), &prior); err != nil {
					return false, "", err
				}
				matches = prior.Source != nil && prior.BaseSHA == version.BaseSHA && prior.Source.DiffSHA256 == version.Source.DiffSHA256
			}
		}
		if matches && version.Source != nil {
			var bundle []byte
			err := q.QueryRowContext(ctx, "SELECT bundle FROM change_sources WHERE version_id=?", version.ID).Scan(&bundle)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, "", err
			}
			if err == nil && version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle) == nil {
				return true, "Verified retained Change source is available", nil
			}
			if checkpoint == nil {
				owner, ownerRunner = sourceOwner, sourceRunner
			}
			if owner == "" {
				return false, "Preserved source owner is unavailable; locate the exact checkpoint before recovery", nil
			}
			if checkpoint == nil || checkpoint.HeadSHA != version.HeadSHA || checkpoint.Availability != "available" || checkpoint.Storage != "local_only" {
				return false, fmt.Sprintf("Change %s version %s retained source is missing or corrupt; verify the local checkpoint on source runner %s before recapture", change.ID, version.ID, owner), nil
			}
			return machine == owner && (ownerRunner == "" || ownerRunner == scope.credential.Runner.RunnerID), fmt.Sprintf("Change %s version %s retained source is missing or corrupt; verify and recapture the exact local head on source runner %s", change.ID, version.ID, owner), nil
		}
		if checkpoint == nil {
			owner, ownerRunner, attempt = sourceOwner, sourceRunner, version.AttemptID
		}
	}
	if checkpoint == nil && (!found || change.CurrentVersion == "") {
		return true, "No preserved source requires runner ownership", nil
	}
	if owner == "" {
		return false, "Preserved source owner is unavailable; locate the exact checkpoint before recovery", nil
	}
	if checkpoint != nil && (checkpoint.Availability == "missing" || checkpoint.Availability == "inaccessible" || checkpoint.Storage == "customer_store") {
		return false, fmt.Sprintf("Checkpoint %s on source runner %s is %s; restore and verify source before recovery", attempt, owner, checkpoint.Availability), nil
	}
	return machine == owner && (ownerRunner == "" || ownerRunner == scope.credential.Runner.RunnerID), fmt.Sprintf("Checkpoint %s is local to source runner %s; wait for that runner to be online, eligible and available, or capture the exact source there before transfer", attempt, owner), nil
}
