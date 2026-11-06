package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const maxRunnerProblemRows = 128

func (s *session) RunnerProblems(ctx context.Context, runnerID string) ([]runnerauth.Problem, error) {
	rows, err := s.db.Query(ctx, `SELECT code,project_id,subject,check_name,error_output,fix_command,message,fix_hint,reported_at,first_seen
		FROM runner_problem_reports WHERE organization_id=$1 AND runner_id=$2
		ORDER BY code,project_id,subject,check_name LIMIT $3`, s.organization, runnerID, maxRunnerProblemRows+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	problems := make([]runnerauth.Problem, 0, maxRunnerProblemRows)
	for rows.Next() {
		var problem runnerauth.Problem
		if err := rows.Scan(&problem.Code, &problem.ProjectID, &problem.Subject, &problem.Check, &problem.ErrorOutput, &problem.FixCommand, &problem.Message, &problem.FixHint, &problem.ReportedAt, &problem.FirstSeen); err != nil {
			return nil, mapError(ctx, err)
		}
		if len(problems) == maxRunnerProblemRows {
			return nil, corestore.ErrInvalidPage
		}
		problems = append(problems, problem)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	return problems, nil
}

func (s *session) ReplaceRunnerProblems(ctx context.Context, runnerID string, problems []runnerauth.Problem) error {
	if runnerID == "" || len(problems) > maxRunnerProblemRows {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM runner_problem_reports WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
		return mapError(ctx, err)
	}
	for _, problem := range problems {
		if problem.Code == "" || len(problem.ProjectID) > 200 || len(problem.Subject) > 2000 || len(problem.Check) > 2000 || len(problem.ErrorOutput) > 2000 || len(problem.FixCommand) > 2000 || len(problem.Message) == 0 || len(problem.Message) > 1000 || len(problem.FixHint) == 0 || len(problem.FixHint) > 1000 || problem.ReportedAt.IsZero() || problem.FirstSeen.IsZero() {
			return runnerstore.ErrInvalidRecord
		}
		_, err := s.db.Exec(ctx, `INSERT INTO runner_problem_reports
			(organization_id,runner_id,code,project_id,subject,check_name,error_output,fix_command,message,fix_hint,reported_at,first_seen)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, s.organization, runnerID, problem.Code, problem.ProjectID, problem.Subject, problem.Check, problem.ErrorOutput, problem.FixCommand, problem.Message, problem.FixHint, problem.ReportedAt, problem.FirstSeen)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
