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
