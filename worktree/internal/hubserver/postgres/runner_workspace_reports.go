package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *session) RunnerWorkspaceReport(ctx context.Context, runnerID string) (workspacesession.Capabilities, string, error) {
	var capabilities workspacesession.Capabilities
	var isolation string
	err := s.db.QueryRow(ctx, `SELECT terminal,files,diff,preview,exec,git,isolation
		FROM runner_workspace_reports WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID).
		Scan(&capabilities.Terminal, &capabilities.Files, &capabilities.Diff, &capabilities.Preview, &capabilities.Exec, &capabilities.Git, &isolation)
	if err != nil {
		return workspacesession.Capabilities{}, "", mapError(ctx, err)
	}
	return capabilities, isolation, nil
}

func (s *session) ReplaceRunnerWorkspaceReport(ctx context.Context, runnerID string, capabilities workspacesession.Capabilities, isolation string) error {
	if runnerID == "" || !workspacesession.ValidIsolation(isolation) {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO runner_workspace_reports (organization_id,runner_id,terminal,files,diff,preview,exec,git,isolation)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(organization_id,runner_id) DO UPDATE SET terminal=EXCLUDED.terminal,files=EXCLUDED.files,diff=EXCLUDED.diff,preview=EXCLUDED.preview,exec=EXCLUDED.exec,git=EXCLUDED.git,isolation=EXCLUDED.isolation
 WHERE (runner_workspace_reports.terminal,runner_workspace_reports.files,runner_workspace_reports.diff,runner_workspace_reports.preview,runner_workspace_reports.exec,runner_workspace_reports.git,runner_workspace_reports.isolation)
 IS DISTINCT FROM (EXCLUDED.terminal,EXCLUDED.files,EXCLUDED.diff,EXCLUDED.preview,EXCLUDED.exec,EXCLUDED.git,EXCLUDED.isolation)`, s.organization, runnerID, capabilities.Terminal, capabilities.Files, capabilities.Diff, capabilities.Preview, capabilities.Exec, capabilities.Git, isolation)
	return mapError(ctx, err)
}

func (s *session) ProjectWorkspaceAvailability(ctx context.Context, projects []tracker.ProjectID, now time.Time) ([]runnerstore.ProjectWorkspaceAvailability, error) {
	if len(projects) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT g.project_id,w.isolation,w.terminal
FROM token_grants g
JOIN runner_identities r ON r.organization_id=g.organization_id AND r.token_id=g.token_id
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
JOIN runner_workspace_reports w ON w.organization_id=r.organization_id AND w.runner_id=r.runner_id
WHERE g.organization_id=$1 AND g.project_id=ANY($2::text[])
AND r.removed_at IS NULL AND r.state='active' AND 'claim'=ANY(r.operations)
AND r.created_at<=$3 AND r.expires_at>$3 AND r.last_heartbeat_at<=$3 AND r.last_heartbeat_at>$4
AND t.revoked_at IS NULL AND t.created_at<=$3 AND (t.expires_at IS NULL OR t.expires_at>$3)
AND w.files AND w.exec`, s.organization, projects, now, now.Add(-runnerauth.HeartbeatTimeout))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var records []runnerstore.ProjectWorkspaceAvailability
	for rows.Next() {
		var record runnerstore.ProjectWorkspaceAvailability
		if err := rows.Scan(&record.ProjectID, &record.Isolation, &record.Terminal); err != nil {
			return nil, mapError(ctx, err)
		}
		records = append(records, record)
	}
	return records, mapError(ctx, rows.Err())
}
