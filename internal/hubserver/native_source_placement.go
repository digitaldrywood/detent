package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// nativeSourceClaimAllowed is part of placement, shared by claim acquisition,
// runner selection and admission reads. A checkout URL is not source evidence.
func nativeSourceClaimAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID) (bool, string, error) {
	var item string
	if err := q.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id=? AND organization_id=? AND project_id=?", id, scope.organization, scope.project).Scan(&item); err != nil {
		return false, "", err
	}
	var owner tracker.MachineID
	var attempt, ownerRunner, raw string
	var checkpoint *tracker.NativeCheckpoint
	// Ignore startup's clean checkout when looking for preserved work. Failed
	// destination startups must not overwrite the source owner's provenance.
	err := q.QueryRowContext(ctx, `SELECT a.id, l.machine_id, COALESCE(lr.runner_id,''), a.checkpoint_json
FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND a.checkpoint_json IS NOT NULL
AND (json_extract(a.checkpoint_json,'$.worktree_state') <> 'clean' OR json_extract(a.checkpoint_json,'$.resume') <> 'fresh_checkout' OR (json_extract(a.checkpoint_json,'$.external_effect') IN ('git_push','pr_create') AND json_extract(a.checkpoint_json,'$.effect_state') IN ('pending','ambiguous')))
ORDER BY a.fencing_token DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&attempt, &owner, &ownerRunner, &raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
			return false, "", err
		}
	}
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
			// An operator can republish the same base/tree under a new head.
			// Prove source equivalence through immutable retained versions;
			// their validation and review receipts remain version-scoped.
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
			return false, fmt.Sprintf("Change %s version %s retained source is missing or corrupt; recover and republish the exact head on source runner %s", change.ID, version.ID, sourceOwner), nil
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
