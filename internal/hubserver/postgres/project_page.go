package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectPage(ctx context.Context, record projectstore.Record) (projectstore.Page, bool, error) {
	if record.Core.OrganizationID != s.organization {
		return projectstore.Page{}, false, corestore.ErrTenantMismatch
	}
	rows, err := s.db.Query(ctx, `SELECT CASE WHEN m.ordinal=0 THEN p.body ELSE '' END,`+"m."+strings.ReplaceAll(projectPartColumns, ",", ",m.")+` FROM project_page_read_model p
 JOIN organizations o ON o.organization_id=p.organization_id
 JOIN project_settings s ON s.organization_id=p.organization_id AND s.project_id=p.project_id
 JOIN project_model_page_parts m ON m.organization_id=p.organization_id AND m.project_id=p.project_id
 WHERE p.organization_id=$1 AND p.project_id=$2 AND p.revision=$3 AND p.source_generation=$4 AND p.workflow_sha256=$5 AND p.model_sha256=$6
 AND p.revision=s.revision AND p.source_generation=s.model_revision AND p.workflow_sha256=s.workflow_sha256 AND p.model_sha256=s.model_sha256 AND p.organization_revision=o.model_selection_revision
 ORDER BY m.ordinal LIMIT 4097`, s.organization, record.Core.ID, record.Settings.Revision, record.Settings.ModelRevision, record.Settings.WorkflowSHA256, record.Settings.ModelSHA256)
	if err != nil {
		return projectstore.Page{}, false, mapError(ctx, err)
	}
	defer rows.Close()
	page := projectstore.Page{}
	parts := []projectstore.ModelPart{}
	for rows.Next() {
		var body string
		part, err := scanProjectPart(rows, &body)
		if err != nil {
			return projectstore.Page{}, false, mapError(ctx, err)
		}
		if len(parts) == 0 {
			page.Body = body
		}
		parts = append(parts, part)
		if len(parts) > projectstore.PagePartLimit {
			return projectstore.Page{}, false, corestore.ErrInvalidPage
		}
	}
	if err := rows.Err(); err != nil {
		return projectstore.Page{}, false, mapError(ctx, err)
	}
	if len(parts) == 0 {
		return projectstore.Page{}, false, nil
	}
	page.Model, err = projectstore.ModelFromParts(parts)
	return page, err == nil, err
}

func (s *session) NextProjectPage(ctx context.Context, after tracker.ProjectID) (projectstore.PageCandidate, error) {
	var c projectstore.PageCandidate
	err := s.db.QueryRow(ctx, `WITH selected AS MATERIALIZED (
 SELECT id FROM projects WHERE organization_id=$1 AND id>$2 AND deleted_at IS NULL ORDER BY id LIMIT 1
 ) SELECT s.project_id,GREATEST(s.page_source_at,o.model_page_source_at),
 COALESCE(p.revision=s.revision AND p.source_generation=s.model_revision AND p.workflow_sha256=s.workflow_sha256 AND p.model_sha256=s.model_sha256 AND p.organization_revision=o.model_selection_revision,false)
 FROM selected n JOIN project_settings s ON s.organization_id=$1 AND s.project_id=n.id
 JOIN organizations o ON o.organization_id=s.organization_id
 LEFT JOIN project_page_read_model p ON p.organization_id=s.organization_id AND p.project_id=s.project_id`, s.organization, after).Scan(&c.ProjectID, &c.SourceAt, &c.Ready)
	if err != nil {
		return c, mapError(ctx, err)
	}
	if c.Ready {
		return c, nil
	}
	c.Record, err = s.ProjectRecord(ctx, c.ProjectID)
	if err != nil {
		return c, err
	}
	c.Models, err = s.OrganizationModels(ctx)
	return c, err
}

func (s *session) PublishProjectPage(ctx context.Context, c projectstore.PageCandidate, page projectstore.Page) (bool, error) {
	r := c.Record
	if r.Core.OrganizationID != s.organization || c.Models.OrganizationID != s.organization {
		return false, corestore.ErrTenantMismatch
	}
	parts, err := projectstore.ModelParts(page.Model)
	if err != nil {
		return false, err
	}
	var project string
	err = s.db.QueryRow(ctx, `SELECT s.project_id FROM project_settings s JOIN organizations o ON o.organization_id=s.organization_id JOIN projects p ON p.organization_id=s.organization_id AND p.id=s.project_id
 WHERE s.organization_id=$1 AND s.project_id=$2 AND s.revision=$3 AND s.model_revision=$4 AND s.workflow_sha256=$5 AND s.model_sha256=$6 AND o.model_selection_revision=$7 AND p.deleted_at IS NULL
 FOR SHARE OF s,o,p`, s.organization, r.Core.ID, r.Settings.Revision, r.Settings.ModelRevision, r.Settings.WorkflowSHA256, r.Settings.ModelSHA256, c.Models.Revision).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError(ctx, err)
	}
	_, err = s.db.Exec(ctx, `INSERT INTO project_page_read_model(organization_id,project_id,revision,source_generation,organization_revision,workflow_sha256,model_sha256,source_at,body)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(organization_id,project_id) DO UPDATE SET revision=EXCLUDED.revision,source_generation=EXCLUDED.source_generation,organization_revision=EXCLUDED.organization_revision,workflow_sha256=EXCLUDED.workflow_sha256,model_sha256=EXCLUDED.model_sha256,source_at=EXCLUDED.source_at,body=EXCLUDED.body,projected_at=clock_timestamp()`, s.organization, r.Core.ID, r.Settings.Revision, r.Settings.ModelRevision, c.Models.Revision, r.Settings.WorkflowSHA256, r.Settings.ModelSHA256, c.SourceAt, page.Body)
	if err != nil {
		return false, mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM project_model_page_parts WHERE organization_id=$1 AND project_id=$2`, s.organization, r.Core.ID); err != nil {
		return false, mapError(ctx, err)
	}
	if err := s.writeProjectParts(ctx, project, parts); err != nil {
		return false, err
	}
	return true, nil
}
