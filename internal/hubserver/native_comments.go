package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeComment(ctx context.Context, query hubapi.NativeQueryer, scope nativeScope, itemID, commentID string) (tracker.NativeComment, error) {
	var comment tracker.NativeComment
	var actor, created, updated string
	var editor, provenance sql.NullString
	err := query.QueryRowContext(ctx, `SELECT id, organization_id, project_id, work_item_id, revision, sequence, body, actor_json, edited_by_json, provenance_json, created_at, updated_at, deleted
FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id = ?`, scope.organization, scope.project, itemID, commentID).Scan(
		&comment.ID, &comment.OrganizationID, &comment.ProjectID, &comment.WorkItemID, &comment.Revision, &comment.Sequence, &comment.Body, &actor, &editor, &provenance, &created, &updated, &comment.Deleted)
	if err != nil {
		return comment, err
	}
	if err := json.Unmarshal([]byte(actor), &comment.Actor); err != nil {
		return comment, err
	}
	if editor.Valid {
		if err := json.Unmarshal([]byte(editor.String), &comment.EditedBy); err != nil {
			return comment, err
		}
	}
	if provenance.Valid {
		if err := json.Unmarshal([]byte(provenance.String), &comment.Provenance); err != nil {
			return comment, err
		}
	}
	if comment.CreatedAt, err = hubapi.ParseTimeValue(created); err != nil {
		return comment, err
	}
	comment.UpdatedAt, err = hubapi.ParseTimeValue(updated)
	return comment, err
}

func (s *Service) createNativeComment(c echo.Context) error {
	var request tracker.CreateComment
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.createNativeCommentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) updateNativeComment(c echo.Context) error {
	var request tracker.UpdateComment
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.updateNativeCommentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("comment"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
