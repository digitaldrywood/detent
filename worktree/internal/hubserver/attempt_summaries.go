package hubserver

import (
	"context"
	"database/sql"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptSummaryContextKey struct{}

func attemptSummaryRead(ctx context.Context) bool {
	value, ok := ctx.Value(attemptSummaryContextKey{}).(bool)
	return ok && value
}

func writeAttemptSummary(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData) error {
	var failureClass, failureStage string
	if data.TerminalFailure != nil {
		failureClass, failureStage = data.TerminalFailure.ErrorClass, data.TerminalFailure.Stage
	}
	_, err := tx.ExecContext(ctx, `UPDATE native_attempts SET machine_id=?,session_id=?,runner_id=?,provider=?,model=?,effort=?,execution_role=?,final_outcome=?,failure_class=?,failure_stage=? WHERE id=?`, data.MachineID, data.SessionID, data.RunnerID, runProvider(data), runModel(data), runEffort(data), activityRunRole(data), data.Outcome, failureClass, failureStage, id)
	return err
}

func writeCheckpointSummary(ctx context.Context, tx *sql.Tx, id string, checkpoint *tracker.NativeCheckpoint) error {
	if checkpoint == nil {
		return nil
	}
	var change, version, head, publication string
	if checkpoint.Change != nil {
		change, version, head = checkpoint.Change.ChangeID, checkpoint.Change.VersionID, checkpoint.Change.HeadSHA
	}
	if checkpoint.Publication != nil {
		publication = checkpoint.Publication.VersionID
	}
	_, err := tx.ExecContext(ctx, `UPDATE native_attempts SET checkpoint_summary_ready=1,checkpoint_resume=?,checkpoint_availability=?,checkpoint_storage=?,checkpoint_worktree_state=?,checkpoint_head_sha=?,checkpoint_workspace_digest=?,checkpoint_expected_head_sha=?,checkpoint_external_effect=?,checkpoint_effect_state=?,checkpoint_effect_id=?,checkpoint_ref=?,checkpoint_commit_sha=?,checkpoint_base_sha=?,checkpoint_tree_sha=?,checkpoint_change_id_summary=?,checkpoint_version_id_summary=?,checkpoint_change_head_sha=?,checkpoint_publication_version_id=? WHERE id=?`, checkpoint.Resume, checkpoint.Availability, checkpoint.Storage, checkpoint.WorktreeState, checkpoint.HeadSHA, checkpoint.WorkspaceDigest, checkpoint.ExpectedHeadSHA, checkpoint.ExternalEffect, checkpoint.EffectState, checkpoint.EffectID, checkpoint.Ref, checkpoint.CommitSHA, checkpoint.BaseSHA, checkpoint.TreeSHA, change, version, head, publication, id)
	return err
}

func readTypedNativeRun(ctx context.Context, q nativeQueryer, id string) (tracker.NativeRunData, *tracker.NativeCheckpoint, error) {
	var run tracker.NativeRunData
	var provider, model, effort, role, failureClass, failureStage string
	var finalization tracker.NativeFinalization
	var disposition tracker.NativeDisposition
	var present, checkpointReady bool
	err := q.QueryRowContext(ctx, `SELECT id,run_id,lease_id,fencing_token,sequence,policy_id,machine_id,session_id,runner_id,provider,model,effort,execution_role,final_outcome,failure_class,failure_stage,finalization_settled,finalization_changed,finalization_change_id,finalization_version_id,coalesce(disposition_present,0),coalesce(disposition_status,''),coalesce(disposition_human_action,0),coalesce(disposition_blockers,0),coalesce(disposition_reason_code,''),coalesce(checkpoint_summary_ready,0) FROM native_attempts WHERE id=?`, id).Scan(&run.AttemptID, &run.RunID, &run.LeaseID, &run.FencingToken, &run.Sequence, &run.PolicyID, &run.MachineID, &run.SessionID, &run.RunnerID, &provider, &model, &effort, &role, &run.Outcome, &failureClass, &failureStage, &finalization.Settled, &finalization.Changed, &finalization.ChangeID, &finalization.VersionID, &present, &disposition.Status, &disposition.HumanAction, &disposition.Blockers, &disposition.ReasonCode, &checkpointReady)
	if err != nil {
		return run, nil, err
	}
	if provider != "" {
		run.Identity = &tracker.NativeExecutionIdentity{Role: role, Backend: provider, Model: model, ReasoningEffort: effort}
	}
	if failureClass != "" || failureStage != "" {
		run.TerminalFailure = &tracker.NativeTerminalFailure{ErrorClass: failureClass, Stage: failureStage}
	}
	if finalization.Settled || finalization.Changed || finalization.ChangeID != "" {
		run.Finalization = &finalization
	}
	if present {
		run.Disposition = &disposition
	}
	if !checkpointReady {
		return run, nil, nil
	}
	checkpoint := &tracker.NativeCheckpoint{}
	var change, version, head, publication string
	err = q.QueryRowContext(ctx, `SELECT checkpoint_resume,checkpoint_availability,checkpoint_storage,checkpoint_worktree_state,checkpoint_head_sha,checkpoint_workspace_digest,checkpoint_expected_head_sha,checkpoint_external_effect,checkpoint_effect_state,checkpoint_effect_id,checkpoint_ref,checkpoint_commit_sha,checkpoint_base_sha,checkpoint_tree_sha,checkpoint_change_id_summary,checkpoint_version_id_summary,checkpoint_change_head_sha,checkpoint_publication_version_id FROM native_attempts WHERE id=?`, id).Scan(&checkpoint.Resume, &checkpoint.Availability, &checkpoint.Storage, &checkpoint.WorktreeState, &checkpoint.HeadSHA, &checkpoint.WorkspaceDigest, &checkpoint.ExpectedHeadSHA, &checkpoint.ExternalEffect, &checkpoint.EffectState, &checkpoint.EffectID, &checkpoint.Ref, &checkpoint.CommitSHA, &checkpoint.BaseSHA, &checkpoint.TreeSHA, &change, &version, &head, &publication)
	if err != nil {
		return run, nil, err
	}
	if change != "" {
		checkpoint.Change = &tracker.NativeChangeReference{ChangeID: change, VersionID: version, HeadSHA: head}
	}
	if publication != "" {
		var p tracker.NativePRPublication
		err = q.QueryRowContext(ctx, `SELECT change_id,version_id,repository,base_ref,branch,head_sha,policy_id,source_attempt_id,provider,external_id,external_url FROM native_pr_publications WHERE version_id=?`, publication).Scan(&p.ChangeID, &p.VersionID, &p.Repository, &p.BaseRef, &p.Branch, &p.HeadSHA, &p.PolicyID, &p.SourceAttemptID, &p.External.Provider, &p.External.ID, &p.External.URL)
		if err != nil {
			return run, nil, err
		}
		p.SourceVersion = tracker.NativeChangeReference{ChangeID: p.ChangeID, VersionID: p.VersionID, HeadSHA: p.HeadSHA}
		checkpoint.Publication = &p
	}
	return run, checkpoint, nil
}

func typedAttemptSelection(query string) string {
	query = strings.ReplaceAll(query, "COALESCE(NULLIF(a.payload_object_key,''),a.data_json)", "a.id")
	query = strings.ReplaceAll(query, "COALESCE(NULLIF(a.checkpoint_object_key,''),a.checkpoint_json)", "NULL")
	return strings.ReplaceAll(query, "a.artifact_ids_json", "'[]'")
}

func readTypedRecoveryChange(ctx context.Context, q nativeQueryer, change tracker.ChangeRequest) (tracker.ChangeDetail, error) {
	detail := tracker.ChangeDetail{Change: change, Summary: tracker.ChangeSummary{Status: "evidence_incomplete", NativeReview: "unknown", ExternalReview: "unknown", Checks: "unknown", Messages: []string{"Read full detail to inspect change evidence"}}}
	if change.CurrentVersion == "" {
		return detail, nil
	}
	var version tracker.ChangeVersion
	var provider, externalID, externalURL, created string
	err := q.QueryRowContext(ctx, `SELECT id,change_id,number,coalesce(attempt_id,''),coalesce(head_sha,''),coalesce(repository,''),coalesce(policy_revision_id,''),coalesce(external_provider,''),coalesce(external_id,''),coalesce(external_url,''),coalesce(created_at,'') FROM change_versions WHERE change_id=? AND id=?`, change.ID, change.CurrentVersion).Scan(&version.ID, &version.ChangeID, &version.Number, &version.AttemptID, &version.HeadSHA, &version.Repository, &version.PolicyID, &provider, &externalID, &externalURL, &created)
	if err != nil {
		return detail, err
	}
	if created != "" {
		version.CreatedAt, err = parseTimeValue(created)
		if err != nil {
			return detail, err
		}
	}
	if provider != "" {
		version.External = &tracker.ChangeExternalReference{Provider: provider, ID: externalID, URL: externalURL}
	}
	detail.Versions = []tracker.ChangeVersion{version}
	return detail, nil
}
