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
	return s.nativeMutation(c, request.Mutation, request, nil)
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
	return s.advanceGitHubImportRecord(ctx, scope, id, revision, key)
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
