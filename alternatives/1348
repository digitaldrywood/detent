package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func migrateDashboardChangeLog(ctx context.Context, tx *sql.Tx) error {
	raw, err := migrationFiles.ReadFile("migration_steps/20261027013142_dashboard_change_log.sql")
	if err != nil {
		return fmt.Errorf("read dashboard log schema: %w", err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	if _, err := tx.ExecContext(ctx, up); err != nil {
		return fmt.Errorf("apply dashboard log schema: %w", err)
	}
	var after string
	for {
		var id, organization, projects string
		err := tx.QueryRowContext(ctx, "SELECT id,organization_id,projects_json FROM health_findings WHERE id>? ORDER BY id LIMIT 1", after).Scan(&id, &organization, &projects)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read legacy finding projects: %w", err)
		}
		var ids []string
		if err := json.Unmarshal([]byte(projects), &ids); err != nil {
			return fmt.Errorf("decode legacy finding projects: %w", err)
		}
		if err := syncHealthFindingProjects(ctx, tx, tracker.OrganizationID(organization), id, ids); err != nil {
			return err
		}
		after = id
	}
}

func syncHealthFindingProjects(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, id string, projects []string) error {
	retained := make(map[string]bool, len(projects))
	for _, project := range projects {
		retained[project] = true
	}
	rows, err := tx.QueryContext(ctx, "SELECT project_id FROM health_finding_projects WHERE finding_id=?", id)
	if err != nil {
		return fmt.Errorf("read finding projects: %w", err)
	}
	defer rows.Close()
	var removed []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return fmt.Errorf("scan finding project: %w", err)
		}
		if !retained[project] {
			removed = append(removed, project)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate finding projects: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close finding projects: %w", err)
	}
	for _, project := range removed {
		if _, err := tx.ExecContext(ctx, "DELETE FROM health_finding_projects WHERE finding_id=? AND project_id=?", id, project); err != nil {
			return fmt.Errorf("remove finding project: %w", err)
		}
	}
	for _, project := range projects {
		if _, err := tx.ExecContext(ctx, "INSERT INTO health_finding_projects(finding_id,organization_id,project_id) VALUES(?,?,?) ON CONFLICT DO NOTHING", id, organization, project); err != nil {
			return fmt.Errorf("insert finding project: %w", err)
		}
	}
	return nil
}
