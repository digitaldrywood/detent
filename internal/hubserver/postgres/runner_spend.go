package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerTokenDelta struct {
	day                             string
	model                           string
	input, cached, output, sessions int64
}

func (s *session) recordRunnerTokenUsage(ctx context.Context, attempt leasestore.Attempt, usage []tracker.NativeUsage) error {
	var project tracker.ProjectID
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE organization_id=$1 AND id=$2`, s.organization, attempt.IssueID).Scan(&project); err != nil {
		return mapError(ctx, err)
	}
	models := map[string]tracker.NativeUsage{}
	for _, entry := range usage {
		combined := models[entry.Model]
		combined.Model = entry.Model
		combined.Input += entry.Input
		combined.CachedInput += entry.CachedInput
		combined.Output += entry.Output
		if combined.To.Before(entry.To) {
			combined.To = entry.To
		}
		models[entry.Model] = combined
	}
	names := make([]string, 0, len(models))
	for model := range models {
		names = append(names, model)
	}
	slices.Sort(names)
	var daily, issues []runnerTokenDelta
	for _, name := range names {
		entry := models[name]
		at := entry.To
		if at.IsZero() {
			at = attempt.UpdatedAt
		}
		day := at.UTC().Format(time.DateOnly)
		var old tracker.NativeUsage
		err := s.db.QueryRowContext(ctx, `SELECT input_tokens,cached_input_tokens,output_tokens FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2 AND model=$3 FOR UPDATE`, s.organization, attempt.ID, name).Scan(&old.Input, &old.CachedInput, &old.Output)
		added := int64(0)
		if errors.Is(err, sql.ErrNoRows) {
			added = 1
		} else if err != nil {
			return mapError(ctx, err)
		}
		delta := runnerTokenDelta{day: day, model: name, input: entry.Input - old.Input, cached: entry.CachedInput - old.CachedInput, output: entry.Output - old.Output, sessions: added}
		issues = append(issues, delta)
		daily = append(daily, delta)
		_, err = s.db.ExecContext(ctx, `INSERT INTO runner_attempt_token_usage(organization_id,attempt_id,model,project_id,issue_id,usage_day,observed_at,input_tokens,cached_input_tokens,output_tokens) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(organization_id,attempt_id,model) DO UPDATE SET usage_day=excluded.usage_day,observed_at=excluded.observed_at,input_tokens=excluded.input_tokens,cached_input_tokens=excluded.cached_input_tokens,output_tokens=excluded.output_tokens`, s.organization, attempt.ID, name, project, attempt.IssueID, day, at, entry.Input, entry.CachedInput, entry.Output)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	slices.SortFunc(daily, func(a, b runnerTokenDelta) int {
		if a.day < b.day {
			return -1
		}
		if a.day > b.day {
			return 1
		}
		if a.model < b.model {
			return -1
		}
		if a.model > b.model {
			return 1
		}
		return 0
	})
	for _, delta := range daily {
		_, err := s.db.ExecContext(ctx, `INSERT INTO runner_project_daily_tokens(organization_id,project_id,usage_day,model,input_tokens,cached_input_tokens,output_tokens,sessions) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id,project_id,usage_day,model) DO UPDATE SET input_tokens=runner_project_daily_tokens.input_tokens+excluded.input_tokens,cached_input_tokens=runner_project_daily_tokens.cached_input_tokens+excluded.cached_input_tokens,output_tokens=runner_project_daily_tokens.output_tokens+excluded.output_tokens,sessions=runner_project_daily_tokens.sessions+excluded.sessions`, s.organization, project, delta.day, delta.model, delta.input, delta.cached, delta.output, delta.sessions)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	for _, delta := range issues {
		_, err := s.db.ExecContext(ctx, `INSERT INTO runner_issue_tokens(organization_id,project_id,issue_id,model,input_tokens,cached_input_tokens,output_tokens,sessions) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id,project_id,issue_id,model) DO UPDATE SET input_tokens=runner_issue_tokens.input_tokens+excluded.input_tokens,cached_input_tokens=runner_issue_tokens.cached_input_tokens+excluded.cached_input_tokens,output_tokens=runner_issue_tokens.output_tokens+excluded.output_tokens,sessions=runner_issue_tokens.sessions+excluded.sessions`, s.organization, project, attempt.IssueID, delta.model, delta.input, delta.cached, delta.output, delta.sessions)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE native_attempts SET runner_tokens_recorded=true WHERE organization_id=$1 AND id=$2`, s.organization, attempt.ID)
	return mapError(ctx, err)
}

func (s *session) RunnerTokenSpend(ctx context.Context, project tracker.ProjectID, issue string, day *time.Time, reporter *string) (store.TokenSpend, error) {
	result := store.TokenSpend{}
	if reporter != nil && day != nil {
		return result, accessstore.ErrDenied
	}
	var unaccounted bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.project_id=$2 AND ($3='' OR i.native_id=$3) AND ($4::text IS NULL OR ($4<>'' AND i.actor_principal_id=$4)) AND NOT a.runner_tokens_recorded)`, s.organization, project, issue, reporter).Scan(&unaccounted); err != nil {
		return result, mapError(ctx, err)
	}
	if unaccounted {
		return result, leasestore.ErrUnaccountedUsage
	}
	var rows *sql.Rows
	var err error
	if day != nil {
		result.Date = day.UTC().Format(time.DateOnly)
		rows, err = s.db.QueryContext(ctx, `SELECT model,input_tokens,cached_input_tokens,output_tokens,sessions FROM runner_project_daily_tokens WHERE organization_id=$1 AND project_id=$2 AND usage_day=$3 ORDER BY model LIMIT 201`, s.organization, project, result.Date)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT u.model,u.input_tokens,u.cached_input_tokens,u.output_tokens,u.sessions FROM runner_issue_tokens u JOIN issues i ON i.organization_id=u.organization_id AND i.id=u.issue_id WHERE u.organization_id=$1 AND u.project_id=$2 AND i.native_id=$3 AND ($4::text IS NULL OR ($4<>'' AND i.actor_principal_id=$4)) ORDER BY u.model LIMIT 201`, s.organization, project, issue, reporter)
	}
	if err != nil {
		return result, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		if len(result.ByModel) >= 200 {
			return store.TokenSpend{}, corestore.ErrInvalidPage
		}
		var model store.ModelTokenSpend
		if err := rows.Scan(&model.Model, &model.InputTokens, &model.CachedInputTokens, &model.OutputTokens, &model.Sessions); err != nil {
			return result, mapError(ctx, err)
		}
		model.TotalTokens = model.InputTokens + model.OutputTokens
		result.ByModel = append(result.ByModel, model)
		result.InputTokens += model.InputTokens
		result.CachedInputTokens += model.CachedInputTokens
		result.OutputTokens += model.OutputTokens
		result.TotalTokens += model.TotalTokens
		result.Sessions += model.Sessions
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) AccountRunnerTokenUsage(ctx context.Context, expected leasestore.Attempt, usage []tracker.NativeUsage) error {
	if err := s.lockAttemptScope(ctx, expected.IssueID); err != nil {
		return err
	}
	var sequence int64
	var issueID int64
	var key string
	var recorded bool
	if err := s.db.QueryRowContext(ctx, `SELECT issue_id,sequence,payload_object_key,runner_tokens_recorded FROM native_attempts WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, expected.ID).Scan(&issueID, &sequence, &key, &recorded); err != nil {
		return mapError(ctx, err)
	}
	if issueID != expected.IssueID || sequence != expected.Sequence || key != expected.PayloadObjectKey {
		return corestore.ErrRevisionConflict
	}
	if recorded {
		return nil
	}
	return s.recordRunnerTokenUsage(ctx, expected, usage)
}

func (s *session) RunnerModelTokenQuantiles(ctx context.Context, project tracker.ProjectID, query store.ModelTokenQuantileQuery, reporter *string) (store.ModelTokenQuantiles, error) {
	result := store.ModelTokenQuantiles{Model: query.Model}
	if reporter != nil {
		return result, accessstore.ErrDenied
	}
	if query.Limit < 1 || query.Limit > 200 || query.Model == "" {
		return result, corestore.ErrInvalidPage
	}
	err := s.db.QueryRowContext(ctx, `WITH recent AS (
SELECT input_tokens,cached_input_tokens,output_tokens FROM runner_attempt_token_usage
WHERE organization_id=$1 AND project_id=$2 AND model=$3 ORDER BY observed_at DESC,attempt_id LIMIT $4)
SELECT count(*),coalesce(percentile_disc(0.5) WITHIN GROUP(ORDER BY input_tokens),0),coalesce(percentile_disc(0.9) WITHIN GROUP(ORDER BY input_tokens),0),
coalesce(percentile_disc(0.5) WITHIN GROUP(ORDER BY cached_input_tokens),0),coalesce(percentile_disc(0.9) WITHIN GROUP(ORDER BY cached_input_tokens),0),
coalesce(percentile_disc(0.5) WITHIN GROUP(ORDER BY output_tokens),0),coalesce(percentile_disc(0.9) WITHIN GROUP(ORDER BY output_tokens),0),
coalesce(percentile_disc(0.5) WITHIN GROUP(ORDER BY input_tokens+output_tokens),0),coalesce(percentile_disc(0.9) WITHIN GROUP(ORDER BY input_tokens+output_tokens),0) FROM recent`, s.organization, project, query.Model, query.Limit).Scan(&result.Sessions, &result.P50InputTokens, &result.P90InputTokens, &result.P50CachedInputTokens, &result.P90CachedInputTokens, &result.P50OutputTokens, &result.P90OutputTokens, &result.P50TotalTokens, &result.P90TotalTokens)
	return result, mapError(ctx, err)
}
