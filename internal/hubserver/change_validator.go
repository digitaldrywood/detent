package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateNativeValidatorReview(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, request tracker.ReviewChange) (tracker.MachineID, error) {

	if request.Validator == nil && request.ImplementationReview == nil && request.SecurityAudit == nil {
		return validateValidatorReviewEvidence(scope, change, version, request, validatorReviewEvidence{})
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
	if err != nil {
		return "", err
	}
	var sourceSession sql.NullInt64
	var sourceMachine sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT implementing_session_id,implementing_machine_id FROM change_versions WHERE id=? AND change_id=?`, version.ID, change.ID).Scan(&sourceSession, &sourceMachine); err != nil {
		return "", err
	}
	if !sourceSession.Valid {
		sourceSession.Int64 = -1
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),data_json) FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND lease_id=?", scope.organization, scope.project, change.WorkItemID, request.LeaseID).Scan(&raw); err != nil {
		return "", err
	}
	var current tracker.NativeRunData
	if err := decodeRunDetail(ctx, tx, raw, &current); err != nil {
		return "", err
	}
	reviews, err := changeRows[tracker.ChangeReview](ctx, tx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id = ? AND version_id = ? AND kind = 'review' ORDER BY sequence", change.ID, version.ID)
	if err != nil {
		return "", err
	}
	return validateValidatorReviewEvidence(scope, change, version, request, validatorReviewEvidence{issue: issue, sourceSession: sourceSession.Int64, sourceMachine: sourceMachine.String, current: current, reviews: reviews})
}

func securityAuditRejection(message, field string) error {
	rejection := runnerauth.PublicationRejection{Category: "security_audit", Field: field}
	if !rejection.Valid() {
		rejection.Field = "review"
	}
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, Details: map[string]any{"rejection": rejection}}
}

func securityAuditEvaluationField(evaluation securityaudit.Evaluation) string {
	if evaluation.Reason == securityaudit.ReasonDiffTooLarge {
		return "run.diff_bytes"
	}
	field, _, _ := strings.Cut(evaluation.Detail, " ")
	if (runnerauth.PublicationRejection{Category: "security_audit", Field: field}).Valid() {
		return field
	}
	return "run"
}
