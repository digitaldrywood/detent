package hubserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) registerAttachmentArchiveRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	base := attachmentMetadataBase + "/archives/:item"
	e.POST(base+"/check", s.checkAttachmentArchive, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.POST(base, s.recordAttachmentArchive, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.GET(base+"/:digest", s.readAttachmentArchive, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
}

func (s *Service) checkAttachmentArchive(c echo.Context) error {
	var input struct {
		Reference *attachment.ArchiveReference `json:"reference,omitempty"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	if input.Reference != nil {
		return s.writeAttachmentArchive(c, *input.Reference, false)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	issue, _, err := readNativeIssue(ctx, s.database.reader, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := requireReporterTriage(scope, issue); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.credential.ID})
}

func (s *Service) recordAttachmentArchive(c echo.Context) error {
	var ref attachment.ArchiveReference
	if err := decodeAPIJSON(c, &ref); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.writeAttachmentArchive(c, ref, true)
}

func (s *Service) writeAttachmentArchive(c echo.Context, ref attachment.ArchiveReference, publish bool) error {
	scope := nativeRequestScope(c)
	if ref.Validate(string(scope.organization)) != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid archive reference"))
	}
	ctx := c.Request().Context()
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	scope.action = operatortool.UploadAttachment
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := requireReporterTriage(scope, issue); err != nil {
		return s.nativeAPIError(c, err)
	}
	now := s.config.now()
	before, err := s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	id := "att_" + artifact.Digest([]byte(string(scope.organization) + "\n" + ref.Key))[:32]
	if !publish {
		_, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version)
VALUES(?,?,?,?,'archive.gz','application/gzip',?,?,?,'archive',?,?,?,?,?) ON CONFLICT DO NOTHING`, id, scope.organization, scope.project, scope.credential.ID, ref.CompressedSize, ref.SHA256, formatHubTime(now), ref.Retention, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	stored, err := scanAttachmentArchive(tx.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, id, scope.organization))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if stored != ref {
		return s.nativeAPIError(c, nativeConflict(0))
	}
	if publish {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_archive_references(organization_id,project_id,work_item_id,attachment_id) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, scope.organization, scope.project, c.Param("item"), id); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes"); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	if !publish {
		return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.credential.ID})
	}
	return c.JSON(http.StatusCreated, ref)
}

func scanAttachmentArchive(row interface{ Scan(...any) error }) (attachment.ArchiveReference, error) {
	var ref attachment.ArchiveReference
	err := row.Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention)
	return ref, err
}

func (s *Service) readAttachmentArchive(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	if !artifact.ValidHash(c.Param("digest"), 64) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if _, _, err := readNativeIssue(ctx, s.database.reader, scope, c.Param("item")); err != nil {
		return s.nativeAPIError(c, err)
	}
	section := c.QueryParam("section")
	if section == "" {
		var count int
		if err := s.database.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_archive_sections r JOIN attachments a ON a.id=r.attachment_id WHERE r.organization_id=? AND r.project_id=? AND r.work_item_id=? AND a.sha256=?`, scope.organization, scope.project, c.Param("item"), c.Param("digest")).Scan(&count); err != nil {
			return s.nativeAPIError(c, err)
		}
		if count != 0 {
			return s.nativeAPIError(c, nativeNotFound())
		}
	}

	if scope.credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if section != "" {
		descriptor, err := readIssueArchiveSection(ctx, s.database.reader, scope, c.Param("item"), section)
		if err != nil || descriptor.SHA256 != c.Param("digest") || strconv.FormatInt(descriptor.Revision, 10) != c.QueryParam("revision") {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if strings.HasPrefix(section, "conversation:") {
			if _, err := s.conversations.loadConversation(ctx, s.database.reader, scope, strings.TrimPrefix(section, "conversation:")); err != nil {
				return s.nativeAPIError(c, err)
			}
		}
	}

	ref, err := scanAttachmentArchive(s.database.reader.QueryRowContext(ctx, `SELECT a.object_key,a.sha256,a.size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention_class
FROM attachments a JOIN attachment_archive_references r ON r.attachment_id=a.id
WHERE r.organization_id=? AND r.project_id=? AND r.work_item_id=? AND a.organization_id=r.organization_id AND a.kind='archive' AND a.sha256=?`, scope.organization, scope.project, c.Param("item"), c.Param("digest")))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if ref.Validate(string(scope.organization)) != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, ref)
}
