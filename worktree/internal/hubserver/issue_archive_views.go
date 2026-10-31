package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type issueArchiveSection struct {
	OrganizationID   tracker.OrganizationID `json:"organization_id"`
	ProjectID        tracker.ProjectID      `json:"project_id"`
	WorkItemID       string                 `json:"work_item_id"`
	Revision         int64                  `json:"revision,string"`
	SectionID        string                 `json:"section_id"`
	SHA256           string                 `json:"sha256"`
	CompressedSize   int64                  `json:"compressed_size"`
	UncompressedSize int64                  `json:"uncompressed_size"`
	Codec            string                 `json:"codec"`
	SchemaVersion    int                    `json:"schema_version"`
	AccessPath       string                 `json:"access_path"`
}

func readIssueArchiveSection(ctx context.Context, q nativeQueryer, scope nativeScope, item, section string) (issueArchiveSection, error) {
	result := issueArchiveSection{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, SectionID: section}
	var key, kind, retention string
	err := q.QueryRowContext(ctx, `SELECT r.archive_revision,a.object_key,a.sha256,a.size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention_class
FROM issue_archive_sections r JOIN attachments a ON a.id=r.attachment_id AND a.organization_id=r.organization_id
WHERE r.organization_id=? AND r.project_id=? AND r.work_item_id=? AND r.section_id=?
AND r.archive_revision=(SELECT MAX(archive_revision) FROM issue_archive_sections WHERE organization_id=r.organization_id AND project_id=r.project_id AND work_item_id=r.work_item_id)`, scope.organization, scope.project, item, section).
		Scan(&result.Revision, &key, &result.SHA256, &result.CompressedSize, &result.UncompressedSize, &result.Codec, &result.SchemaVersion, &kind, &retention)
	if err != nil {
		return result, err
	}
	ref := attachment.ArchiveReference{Key: key, SHA256: result.SHA256, CompressedSize: result.CompressedSize, UncompressedSize: result.UncompressedSize, Codec: result.Codec, SchemaVersion: result.SchemaVersion, Kind: kind, Retention: retention}
	if ref.Validate(string(scope.organization)) != nil || result.SchemaVersion != 1 {
		return result, nativeNotFound()
	}
	result.AccessPath = "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/archives/" + result.SHA256 + "/access?section=" + url.QueryEscape(section) + "&revision=" + strconv.FormatInt(result.Revision, 10)
	return result, nil
}

func (s *Service) archiveView(c echo.Context, item, section string, current any) (bool, error) {
	if c.Request().Header.Get("X-Detent-Archive-Read") != "1" || c.QueryParam("view") != "" {
		return false, nil
	}
	ctx, scope := c.Request().Context(), nativeRequestScope(c)
	if scope.credential.HostedRole == "reporter" {
		return false, nil
	}
	sectionRef, err := readIssueArchiveSection(ctx, s.database.reader, scope, item, section)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true, s.nativeAPIError(c, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := s.database.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM issue_archive_sections WHERE organization_id=? AND project_id=? AND work_item_id=?)", scope.organization, scope.project, item).Scan(&exists); err != nil {
			return true, s.nativeAPIError(c, err)
		}
		if !exists {
			return false, nil
		}
	}
	issue, _, readErr := readNativeIssueProjection(ctx, s.database.reader, scope, item, true)
	if readErr != nil {
		return true, s.nativeAPIError(c, readErr)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true, s.nativeAPIError(c, &nativeError{Code: "archive_unavailable", Message: "Archived detail is unavailable", status: http.StatusServiceUnavailable, publicMessage: true})
	}
	if err != nil {
		return true, s.nativeAPIError(c, err)
	}
	projection := "replace"
	if strings.HasPrefix(section, "change:") {
		change, err := readChange(ctx, s.database.reader, scope, item, strings.TrimPrefix(section, "change:"))
		if err != nil {
			return true, s.nativeAPIError(c, err)
		}
		current = map[string]any{"change": change}
	}
	if strings.HasPrefix(section, "revision::") {
		projection = "revision"
		current = s.nativeIssueResponse(issue)
	}
	if section == "issue" {
		projection = "issue"
		current = s.nativeIssueResponse(issue)
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return true, c.JSON(http.StatusOK, struct {
		Archive    issueArchiveSection `json:"archive"`
		Current    any                 `json:"current"`
		Projection string              `json:"projection"`
	}{sectionRef, current, projection})
}

func (s *Service) archiveConversationView(c echo.Context) (bool, error) {
	if c.Request().Header.Get("X-Detent-Archive-Read") != "1" {
		return false, nil
	}
	scope, ctx := nativeRequestScope(c), c.Request().Context()
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return true, s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	var record conversationRecord
	if item := c.Param("item"); item != "" {
		record, err = s.conversations.readLinkedConversation(ctx, tx, scope, item)
	} else {
		record, err = s.conversations.loadConversation(ctx, tx, scope, c.Param("conversation"))
	}
	if err != nil {
		return true, s.nativeAPIError(c, err)
	}
	if _, err = s.conversations.loadConversation(ctx, tx, scope, record.ID); err != nil {
		return true, s.nativeAPIError(c, err)
	}
	if record.WorkItemID == "" {
		return false, nil
	}
	return s.archiveView(c, record.WorkItemID, "conversation:"+record.ID, map[string]any{"conversation": projectConversation(record)})
}
