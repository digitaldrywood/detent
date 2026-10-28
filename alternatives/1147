package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptClaimSource struct {
	data       string
	checkpoint sql.NullString
}

func updateAttemptClaimFacts(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData) error {
	return writeAttemptClaimFacts(ctx, tx, id, data, data.Handoff != nil, nil)
}

func writeAttemptClaimFacts(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData, checkpointKnown bool, source *attemptClaimSource) error {
	var disposition tracker.NativeDisposition
	if data.Disposition != nil {
		disposition = *data.Disposition
	}
	var acceptance bool
	var landing tracker.NativeLandingReceipt
	if data.Runtime != nil {
		if data.Runtime.Completion != nil {
			acceptance = data.Runtime.Completion.AcceptanceRecorded
		}
		if data.Runtime.Landing != nil {
			landing = *data.Runtime.Landing
		}
	}
	var checkpoint tracker.NativeCheckpoint
	if data.Handoff != nil {
		checkpoint = *data.Handoff
	}
	statement := `UPDATE native_attempts SET policy_id=?,claim_facts_known=CASE WHEN ? OR checkpoint_json IS NULL OR checkpoint_external_effect IS NOT NULL THEN 1 ELSE NULL END,disposition_status=?,disposition_human_action=?,disposition_present=?,disposition_blockers=?,disposition_reason_code=?,acceptance_recorded=?,landing_waiting=?,landing_change_id=?,landing_version_id=?,landing_head_sha=?,checkpoint_external_effect=CASE WHEN ? THEN ? WHEN checkpoint_json IS NULL THEN '' ELSE checkpoint_external_effect END,checkpoint_effect_state=CASE WHEN ? THEN ? WHEN checkpoint_json IS NULL THEN '' ELSE checkpoint_effect_state END,checkpoint_preserves_source=CASE WHEN ? THEN ? ELSE checkpoint_preserves_source END WHERE id=?`
	args := []any{data.PolicyID, checkpointKnown, disposition.Status, disposition.HumanAction, data.Disposition != nil, disposition.Blockers, disposition.ReasonCode, acceptance, landing.Waiting, landing.ChangeID, landing.VersionID, landing.HeadSHA, checkpointKnown, checkpoint.ExternalEffect, checkpointKnown, checkpoint.EffectState, checkpointKnown, data.Handoff.PreservesSource(), id}
	if source != nil {
		statement += ` AND data_json=? AND checkpoint_json IS ? AND claim_facts_known IS NULL`
		args = append(args, source.data, source.checkpoint)
	}
	_, err := tx.ExecContext(ctx, statement, args...)
	return err
}

func eventClaimFacts(run *tracker.NativeRunData) []any {
	var data tracker.NativeRunData
	if run != nil {
		data = *run
	}
	var checkpoint tracker.NativeCheckpoint
	if data.Handoff != nil {
		checkpoint = *data.Handoff
	}
	var change tracker.NativeChangeReference
	if checkpoint.Change != nil {
		change = *checkpoint.Change
	}
	return []any{data.AttemptID, data.RunID, data.LeaseID, data.FencingToken, data.Sequence, data.PolicyID, change.ChangeID, change.VersionID, change.HeadSHA, checkpoint.ExternalEffect, checkpoint.EffectState}
}

type admissionFactWrite func(*sql.Tx) error

func prepareLegacyAdmissionFact(ctx context.Context, q nativeQueryer) (admissionFactWrite, error) {
	var id, raw string
	var checkpoint sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id,data_json,checkpoint_json FROM native_attempts WHERE claim_facts_known IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw, &checkpoint)
	if err == nil {
		var data tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		if checkpoint.Valid {
			if err := json.Unmarshal([]byte(checkpoint.String), &data.Handoff); err != nil {
				return nil, err
			}
		}
		return func(tx *sql.Tx) error {
			return writeAttemptClaimFacts(ctx, tx, id, data, true, &attemptClaimSource{data: raw, checkpoint: checkpoint})
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	err = q.QueryRowContext(ctx, `SELECT id,record_json FROM change_requests WHERE landed_version_id IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
	if err == nil {
		var change tracker.ChangeRequest
		if err := json.Unmarshal([]byte(raw), &change); err != nil {
			return nil, err
		}
		version, head, err := claimChangeIdentity(ctx, q, id, raw)
		if err != nil {
			return nil, err
		}
		landed := ""
		if change.Landed != nil {
			landed = change.Landed.VersionID
		}
		return func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE change_requests SET claim_version_id=?,claim_head_sha=?,landed_version_id=? WHERE id=? AND record_json=? AND landed_version_id IS NULL`, version, head, landed, id, raw)
			return err
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var frozen sql.NullString
	err = q.QueryRowContext(ctx, `SELECT v.id,v.record_json,(SELECT metadata_json FROM policy_revisions p WHERE p.scope=c.organization_id||'/'||c.project_id AND p.policy_id=v.policy_revision_id) FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE v.gate_github_pull_request IS NULL ORDER BY v.id LIMIT 1`).Scan(&id, &raw, &frozen)
	if err == nil {
		head, github, err := decodeLandingVersionFacts(ctx, q, raw, frozen.String)
		if err != nil {
			return nil, err
		}
		return func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE change_versions SET claim_head_sha=?,gate_github_pull_request=? WHERE id=? AND record_json=? AND gate_github_pull_request IS NULL`, head, github, id, raw)
			return err
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var scope string
	err = q.QueryRowContext(ctx, `SELECT scope,policy_id,metadata_json FROM policy_revisions WHERE gate_github_pull_request IS NULL ORDER BY scope,policy_id LIMIT 1`).Scan(&scope, &id, &raw)
	if err == nil {
		var descriptor policy.Descriptor
		if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
			return nil, err
		}
		return func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE policy_revisions SET gate_github_pull_request=? WHERE scope=? AND policy_id=? AND metadata_json=? AND gate_github_pull_request IS NULL`, descriptor.Gates.GitHubPullRequest, scope, id, raw)
			return err
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	err = q.QueryRowContext(ctx, `SELECT id,data_json FROM collaboration_events WHERE claim_attempt_id IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
	if err == nil {
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		facts := eventClaimFacts(data.Run)
		return func(tx *sql.Tx) error {
			args := append(facts, id, raw)
			_, err := tx.ExecContext(ctx, `UPDATE collaboration_events SET claim_attempt_id=?,run_id=?,run_lease_id=?,run_fencing_token=?,run_sequence=?,run_policy_id=?,push_change_id=?,push_version_id=?,push_head_sha=?,push_external_effect=?,push_effect_state=? WHERE id=? AND data_json=? AND claim_attempt_id IS NULL`, args...)
			return err
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return nil, sql.ErrNoRows
}

func answeredClaimKind(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint) string {
	disposition := data.Disposition
	publication := checkpoint != nil && (checkpoint.ExternalEffect == "git_push" || checkpoint.ExternalEffect == "pr_create")
	if publication && (disposition == nil || checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") {
		return ""
	}
	if disposition != nil {
		if disposition.Status == "in_progress" && !disposition.Blockers && !disposition.HumanAction && disposition.ReasonCode == "" {
			return ""
		}
		if disposition.HumanAction {
			return "human_action"
		}
		if disposition.ReasonCode == "permission_wait" {
			return "permission_wait"
		}
	}
	if data.Runtime != nil && data.Runtime.Completion != nil && data.Runtime.Completion.AcceptanceRecorded {
		return "recorded_acceptance"
	}
	return ""
}

func readUnknownAnsweredClaim(ctx context.Context, q nativeQueryer, id tracker.WorkItemID) (tracker.NativeAnsweredExclusion, error) {
	var result tracker.NativeAnsweredExclusion
	var raw, checkpointRaw string
	err := q.QueryRowContext(ctx, `SELECT a.id,a.data_json,COALESCE(a.checkpoint_json,'null') FROM issues i JOIN projects p ON p.id=i.project_id AND p.organization_id=i.organization_id JOIN workflow_states ws ON ws.id=i.workflow_state_id JOIN native_attempts a ON a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id WHERE i.id=? AND p.profile='native' AND lower(trim(ws.detent_state))<>'merging' AND a.claim_facts_known IS NULL AND a.status='succeeded' AND a.work_item_revision>=i.revision AND a.dispatch_generation>=i.dispatch_generation AND a.fencing_token=(SELECT max(latest.fencing_token) FROM native_attempts latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)`, id).Scan(&result.AttemptID, &raw, &checkpointRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.NativeAnsweredExclusion{}, nil
	}
	if err != nil {
		return result, err
	}
	var data tracker.NativeRunData
	if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
		return result, err
	}
	var checkpoint *tracker.NativeCheckpoint
	if err := json.Unmarshal([]byte(checkpointRaw), &checkpoint); err != nil {
		return result, err
	}
	result.Kind = answeredClaimKind(data, checkpoint)
	return result, nil
}
