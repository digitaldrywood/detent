package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const releaseAuditColumns = `id,repository,commit_sha,release_series,run_id,run_attempt,job_id,run_url,sequence,status,bundle_commit,bundle_digest,allowance_invocations,allowance_input,allowance_output,allowance_cached,invocations,input_tokens,output_tokens,cached_input_tokens,digest,bytes`

func (s *Service) registerReleaseAuditRoutes(e *echo.Echo) {
	authority := s.requireNativeScope(apiScopeOperator)
	e.GET(nativeBase+"/release-audits", s.listReleaseAudits, authority)
	e.GET(nativeBase+"/release-audits/:checkpoint", s.getReleaseAudit, authority)
	e.POST(nativeBase+"/release-audits", s.publishReleaseAudit, authority)
}

func (s *Service) getReleaseAudit(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	checkpoint, err := readReleaseAuditCheckpoint(ctx, s.database.reader, nativeRequestScope(c), c.Param("checkpoint"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, checkpoint)
}

func (s *Service) publishReleaseAudit(c echo.Context) error {
	var request struct {
		tracker.Mutation
		releaseaudit.Checkpoint
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if !request.Valid() || request.Identity.ProjectID != string(scope.project) {
		return s.nativeAPIError(c, nativeInvalid("Invalid release audit checkpoint"))
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		repository, err := policyRepository(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(repository, request.Identity.Repository) {
			return nil, nativeInvalid("Release audit repository does not match the selected project")
		}
		existing, err := readReleaseAuditCheckpoint(ctx, tx, scope, request.ID)
		if err == nil {
			if !reflect.DeepEqual(existing, request.Checkpoint) {
				return nil, nativeConflict(0)
			}
			return request.Checkpoint, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		for _, part := range request.Parts {
			var size int
			var digest, contentType string
			if err := tx.QueryRowContext(ctx, `SELECT size,sha256,content_type FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL`, scope.organization, scope.project, part.AttachmentID).Scan(&size, &digest, &contentType); err != nil {
				return nil, err
			}
			if size != part.Bytes || digest != part.Digest || contentType != "text/plain" {
				return nil, nativeInvalid("Release audit evidence does not match its retained attachment")
			}
		}
		checkpoint := request.Checkpoint
		_, err = tx.ExecContext(ctx, `INSERT INTO release_audit_checkpoints(organization_id,project_id,`+releaseAuditColumns+`,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, scope.organization, scope.project, checkpoint.ID, checkpoint.Identity.Repository, checkpoint.Identity.Commit, checkpoint.Identity.ReleaseSeries, checkpoint.Identity.RunID, checkpoint.Identity.Attempt, checkpoint.Identity.JobID, checkpoint.Identity.RunURL, checkpoint.Sequence, checkpoint.Status, checkpoint.BundleCommit, checkpoint.BundleDigest, checkpoint.Allowance.Invocations, checkpoint.Allowance.Usage.Input, checkpoint.Allowance.Usage.Output, checkpoint.Allowance.Usage.Cached, checkpoint.Invocations, checkpoint.Usage.Input, checkpoint.Usage.Output, checkpoint.Usage.Cached, checkpoint.Digest, checkpoint.Bytes, formatHubTime(now))
		if err != nil {
			return nil, err
		}
		for _, part := range checkpoint.Parts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO release_audit_parts(organization_id,project_id,checkpoint_id,part_index,attachment_id,byte_offset,bytes,digest) VALUES (?,?,?,?,?,?,?,?)`, scope.organization, scope.project, checkpoint.ID, part.Index, part.AttachmentID, part.Offset, part.Bytes, part.Digest); err != nil {
				return nil, err
			}
		}
		return checkpoint, nil
	})
}

func readReleaseAuditCheckpoint(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (releaseaudit.Checkpoint, error) {
	var checkpoint releaseaudit.Checkpoint
	checkpoint.Identity.ProjectID = string(scope.project)
	err := query.QueryRowContext(ctx, `SELECT `+releaseAuditColumns+` FROM release_audit_checkpoints WHERE organization_id=? AND project_id=? AND id=?`, scope.organization, scope.project, id).Scan(&checkpoint.ID, &checkpoint.Identity.Repository, &checkpoint.Identity.Commit, &checkpoint.Identity.ReleaseSeries, &checkpoint.Identity.RunID, &checkpoint.Identity.Attempt, &checkpoint.Identity.JobID, &checkpoint.Identity.RunURL, &checkpoint.Sequence, &checkpoint.Status, &checkpoint.BundleCommit, &checkpoint.BundleDigest, &checkpoint.Allowance.Invocations, &checkpoint.Allowance.Usage.Input, &checkpoint.Allowance.Usage.Output, &checkpoint.Allowance.Usage.Cached, &checkpoint.Invocations, &checkpoint.Usage.Input, &checkpoint.Usage.Output, &checkpoint.Usage.Cached, &checkpoint.Digest, &checkpoint.Bytes)
	if err != nil {
		return checkpoint, err
	}
	checkpoint.Parts, err = readReleaseAuditParts(ctx, query, scope, id)
	return checkpoint, err
}

func readReleaseAuditParts(ctx context.Context, query nativeQueryer, scope nativeScope, id string) ([]releaseaudit.Part, error) {
	rows, err := query.QueryContext(ctx, `SELECT attachment_id,part_index,byte_offset,bytes,digest FROM release_audit_parts WHERE organization_id=? AND project_id=? AND checkpoint_id=? ORDER BY part_index`, scope.organization, scope.project, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []releaseaudit.Part
	for rows.Next() {
		var part releaseaudit.Part
		if err := rows.Scan(&part.AttachmentID, &part.Index, &part.Offset, &part.Bytes, &part.Digest); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, rows.Err()
}

func (s *Service) listReleaseAudits(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	after := c.QueryParam("cursor")
	if len(after) > 64 {
		return s.nativeAPIError(c, nativeInvalid("Invalid release audit cursor"))
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT `+releaseAuditColumns+` FROM release_audit_checkpoints current WHERE organization_id=? AND project_id=? AND id>? AND NOT EXISTS (SELECT 1 FROM release_audit_checkpoints newer WHERE newer.organization_id=current.organization_id AND newer.project_id=current.project_id AND newer.run_id=current.run_id AND newer.run_attempt=current.run_attempt AND newer.job_id=current.job_id AND newer.sequence>current.sequence) ORDER BY id LIMIT 21`, scope.organization, scope.project, after)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	page := releaseaudit.CheckpointPage{Items: []releaseaudit.Checkpoint{}}
	for rows.Next() {
		var checkpoint releaseaudit.Checkpoint
		checkpoint.Identity.ProjectID = string(scope.project)
		if err := rows.Scan(&checkpoint.ID, &checkpoint.Identity.Repository, &checkpoint.Identity.Commit, &checkpoint.Identity.ReleaseSeries, &checkpoint.Identity.RunID, &checkpoint.Identity.Attempt, &checkpoint.Identity.JobID, &checkpoint.Identity.RunURL, &checkpoint.Sequence, &checkpoint.Status, &checkpoint.BundleCommit, &checkpoint.BundleDigest, &checkpoint.Allowance.Invocations, &checkpoint.Allowance.Usage.Input, &checkpoint.Allowance.Usage.Output, &checkpoint.Allowance.Usage.Cached, &checkpoint.Invocations, &checkpoint.Usage.Input, &checkpoint.Usage.Output, &checkpoint.Usage.Cached, &checkpoint.Digest, &checkpoint.Bytes); err != nil {
			return s.nativeAPIError(c, err)
		}
		page.Items = append(page.Items, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(page.Items) > 20 {
		page.Items = page.Items[:20]
		page.Next = page.Items[19].ID
	}
	for index := range page.Items {
		checkpoint := &page.Items[index]
		checkpoint.Parts, err = readReleaseAuditParts(ctx, s.database.reader, scope, checkpoint.ID)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, page)
}
