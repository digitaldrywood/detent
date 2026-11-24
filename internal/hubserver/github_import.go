package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubhosted"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type GitHubImport = githubhosted.Import

type startGitHubImportRequest struct {
	tracker.Mutation
	IssueNumber      int              `json:"issue_number"`
	Restart          bool             `json:"restart"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func (s *Service) startGitHubImport(c echo.Context) error {
	var request startGitHubImportRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func (s *Service) getGitHubImport(c echo.Context) error {
	ctx := c.Request().Context()
	result, err := s.readGitHubImportRecord(ctx, hubscope.NativeRequestScope(c), c.Param("import"))
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
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	id := c.Param("import")
	key := request.IdempotencyKey
	if key == "" {
		// Legacy callers do not supply a retry key; preserve their revision
		// conflict semantics. Explicit keys use the shared durable receipt.
		readCtx := c.Request().Context()
		current, err := s.readGitHubImportRecord(readCtx, hubscope.NativeRequestScope(c), id)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if current.Revision != request.ExpectedRevision {
			return s.nativeAPIError(c, hubapi.NativeConflict(current.Revision))
		}
		key = fmt.Sprintf("advance:%s:%d", id, request.ExpectedRevision)
	}
	result, err := s.advanceGitHubImportCommand(c.Request().Context(), hubscope.NativeRequestScope(c), id, request.ExpectedRevision, key)
	if err != nil {
		var failure *hubapi.NativeError
		if errors.As(err, &failure) && failure.Status == http.StatusTooManyRequests {
			// Read back the import state advanced by this request on the writer.
			current, readErr := s.readGitHubImportRecord(c.Request().Context(), hubscope.NativeRequestScope(c), id)
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			return s.refusedAPIError(c, http.StatusTooManyRequests, failure.Code, "Import retry is not ready", current)
		}
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
func (s *Service) advanceGitHubImportCommand(ctx context.Context, scope hubscope.NativeScope, id string, revision tracker.Revision, key string) (json.RawMessage, error) {
	return s.advanceGitHubImportRecord(ctx, scope, id, revision, key)
}

func (s *Service) listGitHubImportRecords(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
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
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid import cursor"))
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
