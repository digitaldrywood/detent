package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type GitHubImport struct {
	EditSequence    int64            `json:"edit_sequence,string"`
	IntakePending   bool             `json:"intake_pending"`
	ID              string           `json:"import_id"`
	IssueNumber     int              `json:"issue_number"`
	WorkItemID      string           `json:"work_item_id,omitempty"`
	Stage           string           `json:"stage"`
	Cursor          string           `json:"cursor,omitempty"`
	Revision        tracker.Revision `json:"revision,string"`
	Pages           int              `json:"pages"`
	Status          string           `json:"status"`
	Gaps            []string         `json:"gaps"`
	LastError       string           `json:"last_error,omitempty"`
	RetryAfter      *time.Time       `json:"retry_after,omitempty"`
	SourceUpdatedAt string           `json:"source_updated_at,omitempty"`
	ObservedAt      string           `json:"observed_at"`
}

type GitHubImportRequest struct {
	CompareBase       string
	CompareHead       string
	Discovery         *tracker.GitHubDiscovery
	Scheduled         *gate.ScheduledEvidence
	ScheduledWorkflow string
	ScheduledBranch   string
	SourceID          string
	Repository        string
	Profile           string
	IssueNumber       int
	Stage             string
	Cursor            string
}

type GitHubImportRecord struct {
	CurrentDependency *bool              `json:"current_dependency,omitempty"`
	SourceKey         string             `json:"source_key"`
	Kind              string             `json:"kind"`
	Data              json.RawMessage    `json:"data"`
	Provenance        tracker.Provenance `json:"provenance"`
	Body              string             `json:"body,omitempty"`
	DependencyID      string             `json:"dependency_id,omitempty"`
}

type GitHubImportPage struct {
	Commits       []string
	BatchPage     *tracker.GitHubDiscoveryPage
	Scheduled     *gate.ScheduledEvidence
	RetryAt       time.Time
	EditSequence  int64
	EditsFinished bool
	Issue         *IssueSource
	Records       []GitHubImportRecord
	NextCursor    string
	Gaps          []string
}

type ImportBackend interface {
	FetchImportPage(context.Context, GitHubImportRequest) (GitHubImportPage, error)
}

func readGitHubImport(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (GitHubImport, error) {
	var result GitHubImport
	var gaps string
	var retry sql.NullString
	err := query.QueryRowContext(ctx, `SELECT g.id, g.issue_number, COALESCE(g.work_item_id, ''), g.stage, g.cursor, g.revision, g.pages, g.status, g.gaps_json, g.last_error, g.retry_after, g.source_updated_at, g.observed_at, g.intake_pending, g.edit_sequence
FROM github_imports g JOIN projects p ON p.id = g.project_id WHERE p.organization_id = ? AND p.id = ? AND g.id = ?`, scope.organization, scope.project, id).Scan(
		&result.ID, &result.IssueNumber, &result.WorkItemID, &result.Stage, &result.Cursor, &result.Revision, &result.Pages, &result.Status, &gaps, &result.LastError, &retry, &result.SourceUpdatedAt, &result.ObservedAt, &result.IntakePending, &result.EditSequence)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(gaps), &result.Gaps); err != nil {
		return result, err
	}
	if retry.Valid {
		value, err := parseTimeValue(retry.String)
		if err != nil {
			return result, err
		}
		result.RetryAfter = &value
	}
	return result, nil
}

type startGitHubImportRequest struct {
	tracker.Mutation
	IssueNumber      int              `json:"issue_number"`
	Restart          bool             `json:"restart"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func (s *Service) startGitHubImport(c echo.Context) error {
	var request startGitHubImportRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, s.startGitHubImportOperation(request))
}

func (s *Service) startGitHubImportOperation(request startGitHubImportRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return nil
}

func (s *Service) getGitHubImport(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result, err := s.readGitHubImportRecord(ctx, nativeRequestScope(c), c.Param("import"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) advanceGitHubImport(c echo.Context) error {
	var request struct {
		tracker.Mutation
		ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	id := c.Param("import")
	key := request.IdempotencyKey
	if key == "" {
		// Legacy callers do not supply a retry key; preserve their revision
		// conflict semantics. Explicit keys use the shared durable receipt.
		readCtx, cancelRead := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		current, err := s.readGitHubImportRecord(readCtx, nativeRequestScope(c), id)
		cancelRead()
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if current.Revision != request.ExpectedRevision {
			return s.nativeAPIError(c, nativeConflict(current.Revision))
		}
		key = fmt.Sprintf("advance:%s:%d", id, request.ExpectedRevision)
	}
	result, err := s.advanceGitHubImportCommand(c.Request().Context(), nativeRequestScope(c), id, request.ExpectedRevision, key)
	if err != nil {
		var failure *nativeError
		if errors.As(err, &failure) && failure.status == http.StatusTooManyRequests {
			// Read back the import state advanced by this request on the writer.
			current, readErr := s.readGitHubImportRecord(c.Request().Context(), nativeRequestScope(c), id)
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			return s.refusedAPIError(c, http.StatusTooManyRequests, failure.Code, "Import retry is not ready", current)
		}
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
func (s *Service) advanceGitHubImportCommand(ctx context.Context, scope nativeScope, id string, revision tracker.Revision, key string) (json.RawMessage, error) {
	return nil, nativePublicInvalid("GitHub import transport is not configured")
}

func (s *Service) fetchImportPage(ctx context.Context, integration ProjectIntegration, job GitHubImport) (GitHubImportPage, error) {
	return GitHubImportPage{}, nativePublicInvalid("GitHub import transport is not configured")
}

func importGitHubComment(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, record GitHubImportRecord, now time.Time) error {
	now = payloadTimestamp(ctx, now)
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM native_comments WHERE project_id = ? AND work_item_id = ? AND source_key = ?", scope.project, item, "github:"+record.Provenance.ExternalID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	comment := tracker.NativeComment{ID: payloadNativeID(ctx, "cmt"), OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(item), Revision: 1, Body: record.Body, Actor: scope.actor(), Provenance: &record.Provenance, CreatedAt: now, UpdatedAt: now}
	if err := tx.QueryRowContext(ctx, "SELECT event_sequence + 1 FROM issues WHERE native_id = ?", item).Scan(&comment.Sequence); err != nil {
		return err
	}
	actor, err := marshalNative(comment.Actor)
	if err != nil {
		return err
	}
	provenance, err := marshalNative(record.Provenance)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_comments (id, organization_id, project_id, work_item_id, revision, sequence, body, actor_json, provenance_json, source_key, created_at, updated_at,change_actor_kind,change_edited_by_kind) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?,?,'')`, comment.ID, scope.organization, scope.project, item, comment.Sequence, comment.Body, actor, provenance, "github:"+record.Provenance.ExternalID, formatHubTime(now), formatHubTime(now), comment.Actor.Kind)
	if err != nil {
		return err
	}
	return recordNativeChange(ctx, tx, scope, comment, item, 1, "comment.imported", tracker.CollaborationData{CommentID: comment.ID, Revision: 1}, now)
}

func (s *Service) listGitHubImportRecords(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	id := c.Param("import")
	if _, err := s.readGitHubImportRecord(ctx, scope, id); err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, cursor, key, err := s.nativePage(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	after, err := strconv.ParseInt("0"+cursor.After, 10, 64)
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid import cursor"))
	}
	records, err := s.readGitHubImportRecords(ctx, scope, id, after, limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result := tracker.Page[GitHubImportRecord]{Items: []GitHubImportRecord{}}
	for _, row := range records {
		if len(result.Items) == limit {
			result.NextCursor, err = encodeNativeCursor(cursor, key)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			break
		}
		record, err := s.decodeGitHubImportRecord(ctx, row)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		result.Items = append(result.Items, record)
		cursor.After = strconv.FormatInt(row.Sequence, 10)
	}
	return c.JSON(http.StatusOK, result)
}
