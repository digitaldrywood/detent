package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) exportNativeWorkItems(c echo.Context) error {
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page, err := s.readBulkExport(c.Request().Context(), nativeRequestScope(c), params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readBulkExport(ctx context.Context, scope nativeScope, params url.Values) (operatortool.WorkBulkExportPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	page := operatortool.WorkBulkExportPage{ProjectID: string(scope.project), GeneratedAt: s.config.now(), Freshness: explain.SourceAvailable, Items: []operatortool.WorkBulkExportItem{}}
	if err := s.requireHostedProject(ctx, s.database.reader, scope, false); err != nil {
		return page, err
	}
	if err := authorizeNativeProject(ctx, s.database.reader, scope); err != nil {
		return page, err
	}
	single := url.Values{}
	for name, values := range params {
		single[name] = values
	}
	single.Del("state")
	single.Del("label")
	if err := validateNativeQuery(single, "open", "archived"); err != nil {
		return page, err
	}
	if err := validateNativeIssueQuery(params); err != nil {
		return page, err
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/export"
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, operatortool.WorkBulkExportMaxItems, operatortool.WorkBulkExportMaxItems)
	if err != nil {
		return page, err
	}
	query, args, err := nativeIssueSelection(scope, params, false)
	if err != nil {
		return page, err
	}
	items, frozen, total, err := s.readIssuePageItems(ctx, query, args, limit, &cursor, "", "")
	if err != nil {
		return page, err
	}
	page.Total = total
	page.Truncated = len(items) > limit
	items = items[:min(len(items), limit)]
	size := 8192
	for _, item := range items {
		issue, _, err := readNativeIssue(ctx, s.database.reader, scope, item.ID)
		if err != nil {
			return page, bulkExportItemFailure(item.ID, "read issue", err)
		}
		exported := operatortool.WorkBulkExportItem{NativeIssue: s.nativeIssueResponse(issue), Comments: []tracker.NativeComment{}}
		ids, err := nativePageIDs(ctx, s.database.reader, "SELECT id FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY sequence", scope.organization, scope.project, item.ID)
		if err != nil {
			return page, bulkExportItemFailure(item.ID, "list comments", err)
		}
		for _, id := range ids {
			comment, err := readNativeComment(ctx, s.database.reader, scope, item.ID, id)
			if err != nil {
				return page, bulkExportItemFailure(item.ID, "read comment "+id, err)
			}
			exported.Comments = append(exported.Comments, comment)
		}
		raw, err := json.Marshal(exported)
		if err != nil {
			return page, bulkExportItemFailure(item.ID, "encode item", err)
		}
		if size+len(raw)+1 > operatortool.WorkBulkExportMaxBytes {
			if len(page.Items) == 0 {
				return page, bulkExportItemFailure(item.ID, "encode item", &operatortool.ResultTooLargeError{SizeBytes: size + len(raw), LimitBytes: operatortool.WorkBulkExportMaxBytes})
			}
			page.Truncated = true
			break
		}
		size += len(raw) + 1
		page.Items = append(page.Items, exported)
		cursor.Issues = &item.Position
	}
	if page.Truncated {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
		if frozen != nil {
			s.issuePages.store(cursor.Issues.Snapshot, cursor.Scope, cursor.Expires, frozen, s.config.now())
		}
	}
	return page, nil
}

func bulkExportItemFailure(id, step string, err error) error {
	return &operatortool.WorkBulkExportItemError{Code: "bulk_export_item_failed", Message: "Work item could not be exported: " + step, WorkItemID: id, Err: fmt.Errorf("%s: %w", step, err)}
}
