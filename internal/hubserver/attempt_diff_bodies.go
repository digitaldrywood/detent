package hubserver

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const diffBodyRetention = 30 * 24 * time.Hour

const supersededDiffBody = `julianday(d.created_at) < julianday(?)
AND NOT EXISTS (SELECT 1 FROM native_attempts a WHERE a.id=d.attempt_id AND a.status='running')
AND NOT EXISTS (SELECT 1 FROM change_versions v JOIN change_requests c ON json_extract(c.record_json,'$.current_version_id')=v.id
WHERE json_extract(v.record_json,'$.attempt_id')=d.attempt_id)
AND d.id <> (SELECT latest.id FROM attempt_diffs latest
WHERE latest.organization_id=d.organization_id AND latest.project_id=d.project_id AND latest.work_item_id=d.work_item_id AND latest.source=d.source
ORDER BY latest.created_at DESC, latest.rowid DESC LIMIT 1)`

func (s *Service) registerAttemptDiffBodyRoutes(e *echo.Echo) {
	e.POST("/internal/v1/diff-bodies/batch", s.attemptDiffBodyBatch)
	e.POST("/internal/v1/diff-bodies/stored", s.attemptDiffBodyStored)
	e.POST("/internal/v1/diff-bodies/deleted", s.attemptDiffBodyDeleted)
	e.POST("/internal/v1/diff-bodies/exists", s.attemptDiffBodyExists)
	e.POST("/internal/v1/diff-bodies/vacuum", s.vacuumAttemptDiffBodies)
}

func diffBodyService(c echo.Context) bool {
	claims, ok := hostedSharedClaims(c)
	return ok && claims.Kind == cloudassert.KindService
}

func (s *Service) attemptDiffBodyBatch(c echo.Context) error {
	if !diffBodyService(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	ctx := c.Request().Context()
	batch := diffbody.Batch{Files: []diffbody.File{}}
	rows, err := s.database.db.QueryContext(ctx, `SELECT d.organization_id,d.project_id,f.diff_id,f.position,f.patch
 FROM attempt_diff_files f JOIN attempt_diffs d ON d.id=f.diff_id WHERE f.patch<>'' ORDER BY f.diff_id,f.position LIMIT 32`)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	var bytes int
	for rows.Next() {
		var file diffbody.File
		if err := rows.Scan(&file.Organization, &file.Project, &file.DiffID, &file.Position, &file.Patch); err != nil {
			return s.nativeAPIError(c, err)
		}
		if bytes+len(file.Patch) > tracker.MaxDiffPatchBytes && len(batch.Files) > 0 {
			break
		}
		file.Body = diffbody.New(file.Organization, file.Project, file.DiffID, file.Position, file.Patch)
		bytes += len(file.Patch)
		batch.Files = append(batch.Files, file)
	}
	if err := rows.Err(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := rows.Close(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(batch.Files) > 0 {
		return c.JSON(http.StatusOK, batch)
	}
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		_, err := tx.ExecContext(ctx, `UPDATE attempt_diff_files SET patch_expired=1 WHERE patch_object<>'' AND patch_expired=0
AND (diff_id,position) IN (SELECT f.diff_id,f.position FROM attempt_diff_files f JOIN attempt_diffs d ON d.id=f.diff_id
WHERE f.patch_object<>'' AND f.patch_expired=0 AND `+supersededDiffBody+` LIMIT 32)`, formatHubTime(now.Add(-diffBodyRetention)))
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	rows, err = s.database.db.QueryContext(ctx, `SELECT d.organization_id,d.project_id,f.diff_id,f.position,f.patch_object,f.patch_sha256,f.patch_size
FROM attempt_diff_files f JOIN attempt_diffs d ON d.id=f.diff_id WHERE f.patch_expired=1 AND f.patch_object<>'' LIMIT 32`)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	for rows.Next() {
		var file diffbody.File
		if err := rows.Scan(&file.Organization, &file.Project, &file.DiffID, &file.Position, &file.Body.Key, &file.Body.SHA256, &file.Body.Bytes); err != nil {
			return s.nativeAPIError(c, err)
		}
		batch.Files = append(batch.Files, file)
	}
	if err := rows.Err(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := rows.Close(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.database.db.QueryRowContext(ctx, `SELECT vacuum_pending FROM attempt_diff_body_migration WHERE singleton=1`).Scan(&batch.VacuumPending); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, batch)
}

func (s *Service) attemptDiffBodyStored(c echo.Context) error {
	if !diffBodyService(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var file diffbody.File
	if err := decodeAPIJSON(c, &file); err != nil {
		return invalidAPIRequest(c, err)
	}
	claims, _ := hostedSharedClaims(c)
	if file.Organization != claims.Audience || file.Body.Validate(file.Organization, file.Project, tracker.MaxDiffPatchBytes) != nil || !validNativeID(file.DiffID, "diff") || file.Position < 0 {
		return s.nativeAPIError(c, nativeInvalid("Invalid migrated diff body"))
	}
	var patch, key string
	if err := s.database.db.QueryRowContext(c.Request().Context(), `SELECT f.patch,f.patch_object FROM attempt_diff_files f JOIN attempt_diffs d ON d.id=f.diff_id
 WHERE f.diff_id=? AND f.position=? AND d.organization_id=? AND d.project_id=?`, file.DiffID, file.Position, file.Organization, file.Project).Scan(&patch, &key); err != nil {
		return s.nativeAPIError(c, err)
	}
	if patch != "" && file.Body != diffbody.New(file.Organization, file.Project, file.DiffID, file.Position, patch) {
		return s.nativeAPIError(c, nativeInvalid("Migrated object must match the stored diff body"))
	}
	if patch == "" && key != file.Body.Key {
		return s.nativeAPIError(c, nativeNotFound())
	}
	result, err := s.database.db.ExecContext(c.Request().Context(), `UPDATE attempt_diff_files SET patch='',patch_object=?,patch_sha256=?,patch_size=?
WHERE diff_id=? AND position=? AND patch<>'' AND length(CAST(patch AS BLOB))=?
AND EXISTS (SELECT 1 FROM attempt_diffs d WHERE d.id=diff_id AND d.organization_id=? AND d.project_id=?)`, file.Body.Key, file.Body.SHA256, file.Body.Bytes, file.DiffID, file.Position, file.Body.Bytes, file.Organization, file.Project)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if count == 0 {
		var key string
		if err := s.database.db.QueryRowContext(c.Request().Context(), `SELECT patch_object FROM attempt_diff_files WHERE diff_id=? AND position=?`, file.DiffID, file.Position).Scan(&key); err != nil || key != file.Body.Key {
			return s.nativeAPIError(c, nativeNotFound())
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) attemptDiffBodyDeleted(c echo.Context) error {
	if !diffBodyService(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	_, err := s.database.db.ExecContext(c.Request().Context(), `UPDATE attempt_diff_files SET patch_object='' WHERE patch_object=? AND patch_expired=1`, input.Key)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) attemptDiffBodyExists(c echo.Context) error {
	if !diffBodyService(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	var exists bool
	if err := s.database.db.QueryRowContext(c.Request().Context(), `SELECT EXISTS(SELECT 1 FROM attempt_diff_files WHERE patch_object=?)`, input.Key).Scan(&exists); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"exists": exists})
}

func (s *Service) vacuumAttemptDiffBodies(c echo.Context) error {
	if !diffBodyService(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	ctx := c.Request().Context()
	var inline int
	if err := s.database.db.QueryRowContext(ctx, `SELECT count(*) FROM attempt_diff_files WHERE patch<>''`).Scan(&inline); err != nil {
		return s.nativeAPIError(c, err)
	}
	if inline != 0 {
		return s.nativeAPIError(c, nativeInvalid("Diff body migration is still pending"))
	}
	if _, err := s.database.db.ExecContext(ctx, `VACUUM`); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, err := s.database.db.ExecContext(ctx, `UPDATE attempt_diff_body_migration SET vacuum_pending=0 WHERE singleton=1`); err != nil {
		return s.nativeAPIError(c, err)
	}
	var bytes int64
	if err := s.database.db.QueryRowContext(ctx, `SELECT coalesce(sum(pgsize),0) FROM dbstat WHERE name='attempt_diff_files'`).Scan(&bytes); err != nil {
		return s.nativeAPIError(c, err)
	}
	s.config.Logger.InfoContext(ctx, "attempt diff body migration complete", "attempt_diff_files_bytes", bytes, "inline_body_bytes", 0)
	return c.JSON(http.StatusOK, map[string]int64{"attempt_diff_files_bytes": bytes, "inline_body_bytes": 0})
}
