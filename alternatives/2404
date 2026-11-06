package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readPendingLandingFact(ctx context.Context, q nativeQueryer, id tracker.WorkItemID) (string, bool, error) {
	var project string
	var pending sql.NullBool
	err := q.QueryRowContext(ctx, `SELECT i.project_id,CASE WHEN c.id IS NULL OR c.claim_version_id='' OR (c.claim_version_id IS NOT NULL AND v.id IS NULL) THEN 0 WHEN c.claim_version_id IS NULL OR c.landed_version_id IS NULL OR v.claim_head_sha IS NULL OR `+nativeLandingGitHubGate+` IS NULL OR EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id AND a.claim_facts_known IS NULL AND a.work_item_revision>=i.revision AND a.dispatch_generation>=i.dispatch_generation AND a.fencing_token=(SELECT max(latest.fencing_token) FROM native_attempts latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)) THEN NULL ELSE (`+nativePendingLandingClause+`) END FROM issues i `+nativeLandingCapacityJoins+` WHERE i.id=?`, id).Scan(&project, &pending)
	if err != nil {
		return "", false, err
	}
	if pending.Valid {
		return project, pending.Bool, nil
	}
	project, err = resolveLandingProject(ctx, q, "?"+strconv.FormatInt(int64(id), 10), true)
	if err != nil || project == "" {
		return project, false, err
	}
	var changeID, version, head, landed string
	prepared := claimPrepared(ctx)
	if prepared != nil {
		fact, ok := prepared.landings["?"+strconv.FormatInt(int64(id), 10)]
		if !ok {
			return "", false, errClaimHintStale
		}
		changeID, version, head, landed = fact.changeID, fact.id, fact.head, fact.landed
	} else {
		var raw string
		if err := q.QueryRowContext(ctx, `SELECT c.id,c.record_json FROM issues i JOIN change_requests c ON c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id WHERE i.id=? ORDER BY c.rowid DESC LIMIT 1`, id).Scan(&changeID, &raw); err != nil {
			return "", false, err
		}
		var change tracker.ChangeRequest
		if err := decodeStorageJSON(ctx, []byte(raw), &change); err != nil {
			return "", false, err
		}
		version = change.CurrentVersion
		if change.Landed != nil {
			landed = change.Landed.VersionID
		}
		if version == "" {
			return project, false, nil
		}
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND id=?`, changeID, version).Scan(&raw); err != nil {
			return "", false, err
		}
		var value tracker.ChangeVersion
		if err := decodeStorageJSON(ctx, []byte(raw), &value); err != nil {
			return "", false, err
		}
		head = value.HeadSHA
	}
	if version == "" || landed == version {
		return project, false, nil
	}
	var raw string
	err = q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(a.payload_object_key,''),a.data_json) FROM issues i JOIN native_attempts a ON a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id WHERE i.id=? AND a.work_item_revision>=i.revision AND a.dispatch_generation>=i.dispatch_generation AND a.fencing_token=(SELECT max(latest.fencing_token) FROM native_attempts latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return project, false, nil
	}
	if err != nil {
		return "", false, err
	}
	var data tracker.NativeRunData
	if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
		return "", false, err
	}
	if data.Runtime == nil || data.Runtime.Landing == nil {
		return project, false, nil
	}
	landing := data.Runtime.Landing
	return project, landing.Waiting && landing.ChangeID == changeID && landing.VersionID == version && landing.HeadSHA == head, nil
}
