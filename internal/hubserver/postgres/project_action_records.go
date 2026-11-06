package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/attachment"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const projectActionRecordColumns = `id,organization_id,project_id,name,keybinding,icon,preview_url,open_preview,run_on_worktree_creation,created_by,revision,created_at,updated_at,command_object_key,command_sha256,command_bytes,command_content_bytes`

func scanProjectActionRecord(row interface{ Scan(...any) error }) (actionstore.Record, error) {
	var r actionstore.Record
	a := &r.Action
	err := row.Scan(&a.ID, &a.OrganizationID, &a.ProjectID, &a.Name, &a.Keybinding, &a.Icon, &a.PreviewURL, &a.OpenPreview, &a.RunOnWorktreeCreation, &a.CreatedBy, &a.Revision, &a.CreatedAt, &a.UpdatedAt, &r.Command.Key, &r.Command.SHA256, &r.Command.CompressedSize, &r.Command.UncompressedSize)
	r.Command.Codec, r.Command.Kind, r.Command.Retention, r.Command.SchemaVersion = "gzip", "archive", attachment.ArchiveRetention, 1
	return r, err
}

func (s *session) ProjectActions(ctx context.Context, project tracker.ProjectID) ([]actionstore.Record, error) {
	rows, err := s.db.Query(ctx, `SELECT `+projectActionRecordColumns+` FROM project_actions WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at,id LIMIT 50`, s.organization, project)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []actionstore.Record{}
	for rows.Next() {
		r, err := scanProjectActionRecord(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, r)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) ProjectAction(ctx context.Context, project tracker.ProjectID, id string) (actionstore.Record, error) {
	r, err := scanProjectActionRecord(s.db.QueryRow(ctx, `SELECT `+projectActionRecordColumns+` FROM project_actions WHERE organization_id=$1 AND project_id=$2 AND id=$3`, s.organization, project, id))
	return r, mapError(ctx, err)
}

func (s *session) CreateProjectAction(ctx context.Context, r actionstore.Record) error {
	a := r.Action
	if a.OrganizationID != string(s.organization) {
		return corestore.ErrTenantMismatch
	}
	if r.Command.Validate(string(s.organization)) != nil || a.Revision != 1 {
		return corestore.ErrRevisionConflict
	}
	if err := s.LockWorkItemProject(ctx, tracker.ProjectID(a.ProjectID)); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `INSERT INTO project_actions (`+projectActionRecordColumns+`,slot) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,free.slot FROM (SELECT candidate AS slot FROM generate_series(0,49) candidate WHERE NOT EXISTS (SELECT 1 FROM project_actions existing WHERE existing.organization_id=$2 AND existing.project_id=$3 AND existing.slot=candidate) ORDER BY candidate LIMIT 1) free`, a.ID, s.organization, a.ProjectID, a.Name, a.Keybinding, a.Icon, a.PreviewURL, a.OpenPreview, a.RunOnWorktreeCreation, a.CreatedBy, a.Revision, a.CreatedAt, a.UpdatedAt, r.Command.Key, r.Command.SHA256, r.Command.CompressedSize, r.Command.UncompressedSize)
	return projectActionWriteResult(ctx, result, err)
}

func (s *session) UpdateProjectAction(ctx context.Context, r actionstore.Record, expected int64) error {
	a := r.Action
	if a.OrganizationID != string(s.organization) {
		return corestore.ErrTenantMismatch
	}
	if r.Command.Validate(string(s.organization)) != nil || a.Revision != expected+1 {
		return corestore.ErrRevisionConflict
	}
	result, err := s.db.Exec(ctx, `UPDATE project_actions SET name=$4,keybinding=$5,icon=$6,preview_url=$7,open_preview=$8,run_on_worktree_creation=$9,revision=$10,updated_at=$11,command_object_key=$12,command_sha256=$13,command_bytes=$14,command_content_bytes=$15 WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND revision=$16`, s.organization, a.ProjectID, a.ID, a.Name, a.Keybinding, a.Icon, a.PreviewURL, a.OpenPreview, a.RunOnWorktreeCreation, a.Revision, a.UpdatedAt, r.Command.Key, r.Command.SHA256, r.Command.CompressedSize, r.Command.UncompressedSize, expected)
	return projectActionWriteResult(ctx, result, err)
}

func (s *session) DeleteProjectAction(ctx context.Context, project tracker.ProjectID, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM project_actions WHERE organization_id=$1 AND project_id=$2 AND id=$3`, s.organization, project, id)
	return projectActionWriteResult(ctx, result, err)
}

func projectActionWriteResult(ctx context.Context, result pgconn.CommandTag, err error) error {
	if err != nil {
		return mapError(ctx, err)
	}
	count := result.RowsAffected()
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
