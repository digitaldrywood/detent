package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func legacyPublicationPush(ctx context.Context, q nativeQueryer, scope nativeScope, item string, version tracker.ChangeVersion) (*tracker.NativeRunData, bool, error) {
	var knownSequence int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT sequence FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='run.checkpointed' AND claim_attempt_id IS NOT NULL AND push_external_effect='git_push' AND push_effect_state='confirmed' AND push_change_id=? AND push_version_id=? AND push_head_sha=? ORDER BY sequence DESC LIMIT 1),0)`, scope.organization, scope.project, item, version.ChangeID, version.ID, version.HeadSHA).Scan(&knownSequence); err != nil {
		return nil, false, err
	}
	rows, err := q.QueryContext(ctx, `SELECT data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='run.checkpointed' AND claim_attempt_id IS NULL AND sequence>? ORDER BY sequence DESC LIMIT ?`, scope.organization, scope.project, item, knownSequence, claimEvidenceLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var records []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, false, err
		}
		records = append(records, raw)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(records) > claimEvidenceLimit {
		return nil, false, fmt.Errorf("legacy publication evidence exceeds bounded preparation: %w", errClaimHintStale)
	}
	for _, raw := range records {
		var data tracker.CollaborationData
		if err := decodeRunHistory(ctx, q, raw, &data); err != nil {
			return nil, false, err
		}
		if data.Run == nil || data.Run.Handoff == nil || data.Run.Handoff.Change == nil {
			continue
		}
		handoff := data.Run.Handoff
		ref := handoff.Change
		if handoff.ExternalEffect != "git_push" || handoff.EffectState != "confirmed" || ref.ChangeID != version.ChangeID || ref.VersionID != version.ID || ref.HeadSHA != version.HeadSHA {
			continue
		}
		valid, err := legacyPublicationProvenance(ctx, q, scope, item, *data.Run)
		if err != nil {
			return nil, true, err
		}
		if !valid {
			return nil, true, nil
		}
		return data.Run, true, nil
	}
	return nil, false, nil
}

func legacyPublicationProvenance(ctx context.Context, q nativeQueryer, scope nativeScope, item string, run tracker.NativeRunData) (bool, error) {
	var raw, policyID string
	var known sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT a.data_json,a.policy_id,a.claim_facts_known FROM native_attempts a JOIN native_attempt_events ae ON ae.attempt_id=a.id AND ae.sequence=? JOIN leases l ON l.lease_id=a.lease_id AND l.fencing_token=a.fencing_token JOIN issues i ON i.id=l.issue_id AND i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id JOIN lease_policies p ON p.lease_id=a.lease_id AND p.scope=a.organization_id||'/'||a.project_id AND p.policy_id=? WHERE a.id=? AND a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND a.run_id=? AND a.lease_id=? AND a.fencing_token=?`, run.Sequence, run.PolicyID, run.AttemptID, scope.organization, scope.project, item, run.RunID, run.LeaseID, run.FencingToken).Scan(&raw, &policyID, &known)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !known.Valid {
		var source tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &source); err != nil {
			return false, err
		}
		policyID = source.PolicyID
	}
	return policyID == run.PolicyID, nil
}
