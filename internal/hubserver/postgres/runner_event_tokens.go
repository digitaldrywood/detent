package postgres

import (
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *runnerEventSession) queueRunnerTokenUsage(attempt leasestore.Attempt, usage []tracker.NativeUsage) error {
	lockKey := fmt.Sprintf("runner-token-models:%d:%s:%d:%s", len(f.session.organization), f.session.organization, len(f.command.Project), f.command.Project)
	f.pending.Queue(`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey)
	models := map[string]tracker.NativeUsage{}
	for _, entry := range usage {
		combined := models[entry.Model]
		combined.Input += entry.Input
		combined.CachedInput += entry.CachedInput
		combined.Output += entry.Output
		if combined.To.Before(entry.To) {
			combined.To = entry.To
		}
		models[entry.Model] = combined
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry := models[name]
		at := entry.To
		if at.IsZero() {
			at = attempt.UpdatedAt
		}
		f.pending.Queue(`WITH old AS MATERIALIZED (
 SELECT input_tokens,cached_input_tokens,output_tokens FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2 AND model=$3 FOR UPDATE
), delta AS MATERIALIZED (
 SELECT $8::bigint-coalesce((SELECT input_tokens FROM old),0) AS input,
 $9::bigint-coalesce((SELECT cached_input_tokens FROM old),0) AS cached,
 $10::bigint-coalesce((SELECT output_tokens FROM old),0) AS output,
 CASE WHEN EXISTS(SELECT 1 FROM old) THEN 0 ELSE 1 END::bigint AS sessions
), written AS (
 INSERT INTO runner_attempt_token_usage(organization_id,attempt_id,model,project_id,issue_id,usage_day,observed_at,input_tokens,cached_input_tokens,output_tokens)
 SELECT $1,$2,$3,$4,$5,$6::date,$7::timestamptz,$8,$9,$10 FROM delta WHERE input>=0 AND cached>=0 AND output>=0
 ON CONFLICT(organization_id,attempt_id,model) DO UPDATE SET usage_day=excluded.usage_day,observed_at=excluded.observed_at,input_tokens=excluded.input_tokens,cached_input_tokens=excluded.cached_input_tokens,output_tokens=excluded.output_tokens RETURNING attempt_id
), daily AS (
 INSERT INTO runner_project_daily_tokens(organization_id,project_id,usage_day,model,input_tokens,cached_input_tokens,output_tokens,sessions)
 SELECT $1,$4,$6::date,CASE WHEN EXISTS(SELECT 1 FROM runner_project_daily_tokens WHERE organization_id=$1 AND project_id=$4 AND usage_day=$6::date AND model=$3) OR NOT EXISTS(SELECT 1 FROM runner_project_daily_tokens WHERE organization_id=$1 AND project_id=$4 AND usage_day=$6::date AND model<>'' OFFSET 199) THEN $3::text ELSE '' END,d.input,d.cached,d.output,d.sessions FROM delta d CROSS JOIN written
 ON CONFLICT(organization_id,project_id,usage_day,model) DO UPDATE SET input_tokens=runner_project_daily_tokens.input_tokens+excluded.input_tokens,cached_input_tokens=runner_project_daily_tokens.cached_input_tokens+excluded.cached_input_tokens,output_tokens=runner_project_daily_tokens.output_tokens+excluded.output_tokens,sessions=runner_project_daily_tokens.sessions+excluded.sessions RETURNING model
), issue AS (
 INSERT INTO runner_issue_tokens(organization_id,project_id,issue_id,model,input_tokens,cached_input_tokens,output_tokens,sessions)
 SELECT $1,$4,$5::bigint,CASE WHEN EXISTS(SELECT 1 FROM runner_issue_tokens WHERE organization_id=$1 AND project_id=$4 AND issue_id=$5::bigint AND model=$3) OR NOT EXISTS(SELECT 1 FROM runner_issue_tokens WHERE organization_id=$1 AND project_id=$4 AND issue_id=$5::bigint AND model<>'' OFFSET 199) THEN $3::text ELSE '' END,d.input,d.cached,d.output,d.sessions FROM delta d CROSS JOIN written
 ON CONFLICT(organization_id,project_id,issue_id,model) DO UPDATE SET input_tokens=runner_issue_tokens.input_tokens+excluded.input_tokens,cached_input_tokens=runner_issue_tokens.cached_input_tokens+excluded.cached_input_tokens,output_tokens=runner_issue_tokens.output_tokens+excluded.output_tokens,sessions=runner_issue_tokens.sessions+excluded.sessions RETURNING model
) SELECT EXISTS(SELECT 1 FROM written)`, f.session.organization, attempt.ID, name, f.command.Project, attempt.IssueID, at.UTC().Format(time.DateOnly), at, entry.Input, entry.CachedInput, entry.Output).QueryRow(func(row pgx.Row) error {
			var valid bool
			if err := row.Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return leasestore.ErrAttemptConflict
			}
			return nil
		})
	}
	f.pending.Queue(`SELECT EXISTS(SELECT 1 FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2 OFFSET 200)`, f.session.organization, attempt.ID).QueryRow(func(row pgx.Row) error {
		var overflow bool
		if err := row.Scan(&overflow); err != nil {
			return err
		}
		if overflow {
			return leasestore.ErrAttemptConflict
		}
		return nil
	})
	f.pending.Queue(`UPDATE native_attempts SET runner_tokens_recorded=true WHERE organization_id=$1 AND id=$2`, f.session.organization, attempt.ID)
	return nil
}
