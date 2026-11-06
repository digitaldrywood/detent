package postgres

import "context"

func (s *session) AttachmentObjectExists(ctx context.Context, id, key string) (bool, error) {
	var exists bool
	if key == "" {
		err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=$1 AND id=$2 AND object_deleted_at IS NULL)`, s.organization, id).Scan(&exists)
		return exists, mapError(ctx, err)
	}
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ai_credit_reason_references WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM archive_objects WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM attachments WHERE organization_id=$1 AND object_key=$2 AND object_deleted_at IS NULL) OR
EXISTS (SELECT 1 FROM attempt_diff_body_references WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM change_evidence WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM change_landing_receipts WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM change_requests WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM change_review_policy_revisions WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM change_versions WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM change_versions WHERE organization_id=$1 AND source_object_key=$2) OR
EXISTS (SELECT 1 FROM collaboration_events WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM collaboration_versions WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM conversation_content_objects WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM cost_import_rows WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM cost_observation_revisions WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM github_import_records WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM health_findings WHERE organization_id=$1 AND evidence_object_key=$2) OR
EXISTS (SELECT 1 FROM hosted_audit WHERE organization_id=$1 AND mutation_object_key=$2) OR
EXISTS (SELECT 1 FROM hosted_billing_audit WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM hosted_billing_retired WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM hosted_plan_audit WHERE organization_id=$1 AND object_key=$2) OR
EXISTS (SELECT 1 FROM issues WHERE organization_id=$1 AND body_object_key=$2) OR
EXISTS (SELECT 1 FROM issues WHERE organization_id=$1 AND issue_contract_object_key=$2) OR
EXISTS (SELECT 1 FROM issues WHERE organization_id=$1 AND report_context_object_key=$2) OR
EXISTS (SELECT 1 FROM linked_issue_sources WHERE organization_id=$1 AND snapshot_object_key=$2) OR
EXISTS (SELECT 1 FROM native_attempts WHERE organization_id=$1 AND artifacts_object_key=$2) OR
EXISTS (SELECT 1 FROM native_attempts WHERE organization_id=$1 AND checkpoint_object_key=$2) OR
EXISTS (SELECT 1 FROM native_attempts WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM native_board_deltas WHERE organization_id=$1 AND current_object_key=$2) OR
EXISTS (SELECT 1 FROM native_board_deltas WHERE organization_id=$1 AND previous_object_key=$2) OR
EXISTS (SELECT 1 FROM native_board_items WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM native_commands WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM native_comments WHERE organization_id=$1 AND body_object_key=$2) OR
EXISTS (SELECT 1 FROM onboarding_issue_intake WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM policy_revisions WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM project_actions WHERE organization_id=$1 AND command_object_key=$2) OR
EXISTS (SELECT 1 FROM projects WHERE organization_id=$1 AND model_selection_object_key=$2) OR
EXISTS (SELECT 1 FROM projects WHERE organization_id=$1 AND states_object_key=$2) OR
EXISTS (SELECT 1 FROM projects WHERE organization_id=$1 AND workflow_object_key=$2) OR
EXISTS (SELECT 1 FROM runner_audit_dispositions WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM runner_session_history WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM runner_sessions WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM search_documents WHERE organization_id=$1 AND aggregate_object_key=$2) OR
EXISTS (SELECT 1 FROM search_documents WHERE organization_id=$1 AND payload_object_key=$2) OR
EXISTS (SELECT 1 FROM team_thread_posts WHERE organization_id=$1 AND body_object_key=$2)`, s.organization, key).Scan(&exists)
	return exists, mapError(ctx, err)
}
