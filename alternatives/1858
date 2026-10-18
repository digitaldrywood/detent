package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeComment(ctx context.Context, query nativeQueryer, scope nativeScope, itemID, commentID string) (tracker.NativeComment, error) {
	var comment tracker.NativeComment
	var actor, created, updated string
	var editor, provenance sql.NullString
	err := query.QueryRowContext(ctx, `SELECT id, organization_id, project_id, work_item_id, revision, sequence, body, actor_json, edited_by_json, provenance_json, created_at, updated_at
FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id = ?`, scope.organization, scope.project, itemID, commentID).Scan(
		&comment.ID, &comment.OrganizationID, &comment.ProjectID, &comment.WorkItemID, &comment.Revision, &comment.Sequence, &comment.Body, &actor, &editor, &provenance, &created, &updated)
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
	if comment.CreatedAt, err = parseTimeValue(created); err != nil {
		return comment, err
	}
	comment.UpdatedAt, err = parseTimeValue(updated)
	return comment, err
}

func (s *Service) createNativeComment(c echo.Context) error {
	var request tracker.CreateComment
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.createNativeCommentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

// insertNativeComment appends a comment by the request's actor to a native
// issue and records it in the issue's history.
func insertNativeComment(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, body string, provenance *tracker.Provenance, now time.Time) (tracker.NativeComment, error) {
	comment := tracker.NativeComment{ID: newNativeID("cmt"), OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID,
		Revision: 1, Body: body, Actor: scope.actor(), Provenance: provenance, CreatedAt: now, UpdatedAt: now}
	if err := tx.QueryRowContext(ctx, "SELECT event_sequence + 1 FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", scope.organization, scope.project, issue.WorkItemID).Scan(&comment.Sequence); err != nil {
		return comment, err
	}
	actor, err := marshalNative(comment.Actor)
	if err != nil {
		return comment, err
	}
	encodedProvenance, err := marshalNative(comment.Provenance)
	if err != nil {
		return comment, err
	}
	var sourceKey any
	if comment.Provenance != nil {
		sourceKey = comment.Provenance.Provider + ":" + comment.Provenance.ExternalID
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO native_comments (id, organization_id, project_id, work_item_id, revision, sequence, body, actor_json, provenance_json, source_key, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)", comment.ID, scope.organization, scope.project, issue.WorkItemID, comment.Sequence, comment.Body, actor, encodedProvenance, sourceKey, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return comment, err
	}
	if err := bindCloudAttachmentReferences(ctx, tx, scope, string(issue.WorkItemID), comment.ID, body); err != nil {
		return comment, err
	}
	if err := recordNativeChange(ctx, tx, scope, comment, string(issue.WorkItemID), comment.Revision, "comment.created", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, now); err != nil {
		return comment, err
	}
	return comment, nil
}

func (s *Service) updateNativeComment(c echo.Context) error {
	var request tracker.UpdateComment
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.updateNativeCommentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("comment"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
