package postgres

import (
	"context"

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
		ON CONFLICT(organization_id,runner_id) DO UPDATE SET terminal=EXCLUDED.terminal,files=EXCLUDED.files,diff=EXCLUDED.diff,preview=EXCLUDED.preview,exec=EXCLUDED.exec,git=EXCLUDED.git,isolation=EXCLUDED.isolation`, s.organization, runnerID, capabilities.Terminal, capabilities.Files, capabilities.Diff, capabilities.Preview, capabilities.Exec, capabilities.Git, isolation)
	return mapError(ctx, err)
}
