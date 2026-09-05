package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
		if err != nil {
			return nil, err
		}
		if issue.Profile != "native" {
			return nil, nativeInvalid("Compatibility project discussion is externally owned")
		}
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return nil, nativeInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if err := validateNativeProvenance(scope, request.Provenance); err != nil {
			return nil, err
		}
		if request.Provenance != nil {
			var existing string
			err := tx.QueryRowContext(ctx, "SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND source_key = ?", scope.organization, scope.project, issue.WorkItemID, request.Provenance.Provider+":"+request.Provenance.ExternalID).Scan(&existing)
			if err == nil {
				return readNativeComment(ctx, tx, scope, string(issue.WorkItemID), existing)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		comment := tracker.NativeComment{ID: newNativeID("cmt"), OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID,
			Revision: 1, Body: request.Body, Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now}
		if err := tx.QueryRowContext(ctx, "SELECT event_sequence + 1 FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", scope.organization, scope.project, issue.WorkItemID).Scan(&comment.Sequence); err != nil {
			return nil, err
		}
		actor, err := marshalNative(comment.Actor)
		if err != nil {
			return nil, err
		}
		provenance, err := marshalNative(comment.Provenance)
		if err != nil {
			return nil, err
		}
		var sourceKey any
		if comment.Provenance != nil {
			sourceKey = comment.Provenance.Provider + ":" + comment.Provenance.ExternalID
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO native_comments (id, organization_id, project_id, work_item_id, revision, sequence, body, actor_json, provenance_json, source_key, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)", comment.ID, scope.organization, scope.project, issue.WorkItemID, comment.Sequence, comment.Body, actor, provenance, sourceKey, formatHubTime(now), formatHubTime(now))
		if err != nil {
			return nil, err
		}
		if err := recordNativeChange(ctx, tx, scope, comment, string(issue.WorkItemID), comment.Revision, "comment.created", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, now); err != nil {
			return nil, err
		}
		return comment, nil
	})
}

func (s *Service) updateNativeComment(c echo.Context) error {
	var request tracker.UpdateComment
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		comment, err := readNativeComment(ctx, tx, scope, c.Param("item"), c.Param("comment"))
		if err != nil {
			return nil, err
		}
		if comment.Actor.PrincipalID != scope.credential.ID && scope.credential.Scope == apiScopeWorker {
			return nil, nativeNotFound()
		}
		if request.ExpectedRevision <= 0 {
			return nil, nativeInvalid("Expected revision must be positive")
		}
		if comment.Revision != request.ExpectedRevision {
			return nil, nativeConflict(comment.Revision)
		}
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return nil, nativeInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		comment.Body = request.Body
		comment.Revision++
		comment.UpdatedAt = now
		editor := scope.actor()
		comment.EditedBy = &editor
		editedBy, err := marshalNative(editor)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE native_comments SET body = ?, revision = ?, edited_by_json = ?, updated_at = ? WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id = ?", comment.Body, comment.Revision, editedBy, formatHubTime(now), scope.organization, scope.project, comment.WorkItemID, comment.ID)
		if err != nil {
			return nil, err
		}
		if err := recordNativeChange(ctx, tx, scope, comment, string(comment.WorkItemID), comment.Revision, "comment.edited", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, now); err != nil {
			return nil, err
		}
		return comment, nil
	})
}
