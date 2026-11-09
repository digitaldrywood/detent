package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const healthEvaluationColumns = `organization_id,id,evaluated_at,input_object_key,input_sha256,input_bytes,input_content_bytes,finding_count,baseline_count,revision,phase,position,finding_cursor,baseline_project_cursor,baseline_signal_cursor,baseline_lane_cursor`

func scanHealthEvaluation(ctx context.Context, row pgx.Row) (healthstore.Evaluation, error) {
	var value healthstore.Evaluation
	var key, digest string
	var compressed, content int64
	err := row.Scan(&value.OrganizationID, &value.ID, &value.At, &key, &digest, &compressed, &content, &value.FindingCount, &value.BaselineCount, &value.Revision, &value.Phase, &value.Position, &value.FindingCursor, &value.BaselineCursor.ProjectID, &value.BaselineCursor.Signal, &value.BaselineCursor.Lane)
	if err != nil {
		return value, mapError(ctx, err)
	}
	value.Input = eventstore.Reference(key, digest, compressed, content)
	return value, nil
}

func (s *session) HealthEvaluation(ctx context.Context) (healthstore.Evaluation, error) {
	return scanHealthEvaluation(ctx, s.db.QueryRow(ctx, `SELECT `+healthEvaluationColumns+` FROM health_evaluation_work WHERE organization_id=$1`, s.organization))
}

func (s *session) StartHealthEvaluation(ctx context.Context, work healthstore.Evaluation) (healthstore.Evaluation, error) {
	if err := s.checkOrganization(work.OrganizationID); err != nil {
		return healthstore.Evaluation{}, err
	}
	if work.ID == "" || work.FindingCount < 0 || work.FindingCount > 10000 || work.BaselineCount < 0 || work.BaselineCount > 10000 || work.At.IsZero() || !work.At.Equal(work.At.UTC().Truncate(time.Microsecond)) || work.Input.Validate(string(s.organization)) != nil || work.Input.BindingID != "" || work.Input.CustomerOwned {
		return healthstore.Evaluation{}, corestore.ErrInvalidPage
	}
	if err := validRecordPayload(s.organization, work.Input.Key, work.Input.SHA256, work.Input.CompressedSize, work.Input.UncompressedSize); err != nil {
		return healthstore.Evaluation{}, err
	}
	row, err := scanHealthEvaluation(ctx, s.db.QueryRow(ctx, `INSERT INTO health_evaluation_work(organization_id,id,evaluated_at,input_object_key,input_sha256,input_bytes,input_content_bytes,finding_count,baseline_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING RETURNING `+healthEvaluationColumns, s.organization, work.ID, work.At, work.Input.Key, work.Input.SHA256, work.Input.CompressedSize, work.Input.UncompressedSize, work.FindingCount, work.BaselineCount))
	if errors.Is(err, corestore.ErrNotFound) {
		return healthstore.Evaluation{}, corestore.ErrRevisionConflict
	}
	if err != nil {
		return healthstore.Evaluation{}, err
	}
	tick, err := s.HealthTick(ctx)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return healthstore.Evaluation{}, err
	}
	if err == nil && !work.At.After(tick) {
		return healthstore.Evaluation{}, corestore.ErrRevisionConflict
	}
	return row, nil
}

func (s *session) LockHealthEvaluation(ctx context.Context, work healthstore.Evaluation) (healthstore.Evaluation, error) {
	if err := s.checkOrganization(work.OrganizationID); err != nil {
		return healthstore.Evaluation{}, err
	}
	if work.Input.Validate(string(s.organization)) != nil {
		return healthstore.Evaluation{}, corestore.ErrTenantMismatch
	}
	row, err := scanHealthEvaluation(ctx, s.db.QueryRow(ctx, `SELECT `+healthEvaluationColumns+` FROM health_evaluation_work WHERE organization_id=$1 AND id=$2 AND revision=$3 AND input_object_key=$4 AND input_sha256=$5 AND evaluated_at=$6 AND finding_count=$7 AND baseline_count=$8 FOR UPDATE`, s.organization, work.ID, work.Revision, work.Input.Key, work.Input.SHA256, work.At, work.FindingCount, work.BaselineCount))
	if errors.Is(err, corestore.ErrNotFound) {
		return healthstore.Evaluation{}, corestore.ErrRevisionConflict
	}
	return row, err
}

func validHealthEvaluationProgress(work healthstore.Evaluation, next healthstore.EvaluationProgress) bool {
	if next.Position < 0 || next.Position > 10000 {
		return false
	}
	if next.Phase == work.Phase {
		switch next.Phase {
		case "publish":
			return next.Position == work.Position+1 && next.Position <= work.FindingCount
		case "resolve":
			return next.Position == 0 && next.FindingCursor != "" && next.FindingCursor != work.FindingCursor
		case "baseline_clear":
			return next.Position == 0 && next.BaselineCursor != work.BaselineCursor
		case "baseline_put":
			return next.Position == work.Position+1 && next.Position <= work.BaselineCount
		}
	}
	if next.Position != 0 || next.FindingCursor != "" || next.BaselineCursor != (healthstore.Baseline{}) {
		return false
	}
	return work.Phase == "publish" && work.Position == work.FindingCount && next.Phase == "resolve" || work.Phase == "resolve" && next.Phase == "baseline_clear" || work.Phase == "baseline_clear" && next.Phase == "baseline_put"
}

func (s *session) AdvanceHealthEvaluation(ctx context.Context, work healthstore.Evaluation, next healthstore.EvaluationProgress) (healthstore.Evaluation, error) {
	if err := s.checkOrganization(work.OrganizationID); err != nil {
		return healthstore.Evaluation{}, err
	}
	if work.Input.Validate(string(s.organization)) != nil {
		return healthstore.Evaluation{}, corestore.ErrTenantMismatch
	}
	if !validHealthEvaluationProgress(work, next) {
		return healthstore.Evaluation{}, corestore.ErrInvalidPage
	}
	row, err := scanHealthEvaluation(ctx, s.db.QueryRow(ctx, `UPDATE health_evaluation_work SET revision=revision+1,phase=$4,position=$5,finding_cursor=$6,baseline_project_cursor=$7,baseline_signal_cursor=$8,baseline_lane_cursor=$9 WHERE organization_id=$1 AND id=$2 AND revision=$3 AND phase=$10 AND position=$11 AND finding_count=$12 AND baseline_count=$13 AND input_object_key=$14 AND input_sha256=$15 AND evaluated_at=$16 AND finding_cursor=$17 AND baseline_project_cursor=$18 AND baseline_signal_cursor=$19 AND baseline_lane_cursor=$20 AND ($4<>phase OR phase IN('publish','baseline_put') OR phase='resolve' AND $6>finding_cursor OR phase='baseline_clear' AND ($7,$8,$9)>(baseline_project_cursor,baseline_signal_cursor,baseline_lane_cursor)) RETURNING `+healthEvaluationColumns, s.organization, work.ID, work.Revision, next.Phase, next.Position, next.FindingCursor, next.BaselineCursor.ProjectID, next.BaselineCursor.Signal, next.BaselineCursor.Lane, work.Phase, work.Position, work.FindingCount, work.BaselineCount, work.Input.Key, work.Input.SHA256, work.At, work.FindingCursor, work.BaselineCursor.ProjectID, work.BaselineCursor.Signal, work.BaselineCursor.Lane))
	if errors.Is(err, corestore.ErrNotFound) {
		return healthstore.Evaluation{}, corestore.ErrRevisionConflict
	}
	return row, err
}

func (s *session) FinishHealthEvaluation(ctx context.Context, work healthstore.Evaluation) error {
	current, err := s.LockHealthEvaluation(ctx, work)
	if err != nil {
		return err
	}
	if current.Phase != "baseline_put" || current.Position != current.BaselineCount {
		return corestore.ErrInvalidPage
	}
	if err := s.PutHealthTick(ctx, PutHealthTickInput{OrganizationID: s.organization, LastTickAt: current.At}); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM health_evaluation_work WHERE organization_id=$1 AND id=$2 AND revision=$3`, s.organization, current.ID, current.Revision)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) HealthEvaluationBaselines(ctx context.Context, after healthstore.Baseline, limit int) ([]healthstore.Baseline, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT project_id,signal,lane FROM health_baseline_unavailable WHERE organization_id=$1 AND (project_id,signal,lane)>($2,$3,$4) ORDER BY project_id,signal,lane LIMIT $5`, s.organization, after.ProjectID, after.Signal, after.Lane, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.Baseline
	for rows.Next() {
		var value healthstore.Baseline
		if err := rows.Scan(&value.ProjectID, &value.Signal, &value.Lane); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, value)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthEvaluationProjects(ctx context.Context, projects []tracker.ProjectID) ([]tracker.ProjectID, error) {
	if len(projects) > 100 {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, len(projects))
	for i, id := range projects {
		ids[i] = string(id)
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=ANY($2::text[]) AND deleted_at IS NULL AND profile='native' ORDER BY id`, s.organization, ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []tracker.ProjectID
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, id)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthEvaluationHasProjectLink(ctx context.Context, finding string, projects []tracker.ProjectID) (bool, error) {
	if len(projects) > 100 {
		return false, corestore.ErrInvalidPage
	}
	ids := make([]string, len(projects))
	for i, id := range projects {
		ids[i] = string(id)
	}
	var found bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM health_finding_projects WHERE organization_id=$1 AND finding_id=$2 AND project_id=ANY($3::text[]))`, s.organization, finding, ids).Scan(&found)
	return found, mapError(ctx, err)
}
