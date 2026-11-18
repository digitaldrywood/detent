package rowlookup

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader struct {
	Organization tracker.OrganizationID
	ForShare     bool
	Scan         func(context.Context, string, []any, ...any) error
}

func (r Reader) AccessOrganization(ctx context.Context) (accessstore.Organization, error) {
	var row accessstore.Organization
	query := `SELECT organization_id,name,slug,local,created_at FROM organizations WHERE organization_id=$1`
	if r.ForShare {
		query += " FOR SHARE"
	}
	err := r.Scan(ctx, query, []any{r.Organization}, &row.OrganizationID, &row.Name, &row.Slug, &row.Local, &row.CreatedAt)
	return row, pgcore.MapError(ctx, err)
}

func (r Reader) AttachmentObjectExists(ctx context.Context, id, key string) (bool, error) {
	var exists bool
	if key == "" {
		err := r.Scan(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=$1 AND id=$2 AND object_deleted_at IS NULL)`, []any{r.Organization, id}, &exists)
		return exists, pgcore.MapError(ctx, err)
	}
	err := r.Scan(ctx, `SELECT EXISTS (SELECT 1 FROM ai_credit_reason_references WHERE organization_id=$1 AND object_key=$2) OR
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
EXISTS (SELECT 1 FROM core_work_events WHERE organization_id=$1 AND payload_object_key=$2) OR
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
EXISTS (SELECT 1 FROM team_thread_posts WHERE organization_id=$1 AND body_object_key=$2)`, []any{r.Organization, key}, &exists)
	return exists, pgcore.MapError(ctx, err)
}
