package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

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
	selection, err := boardRecordSelection(scope, params)
	if err != nil {
		return page, err
	}
	after := int64(0)
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil || after < 0 {
			return page, nativePublicInvalid("Cursor is invalid")
		}
	}
	filter := corestore.WorkItemExportSelection{ProjectID: scope.project, States: selection.States, Labels: selection.Labels, Archived: selection.Archived, Terminal: selection.Terminal, Reporter: selection.Reporter, PrincipalID: scope.actor().PrincipalID}
	size := 8192
	for len(page.Items) < limit {
		filter.IncludeTotal = len(page.Items) == 0
		batchSize := min(corestore.MaxPageSize-1, limit-len(page.Items))
		batchLimit := batchSize + 1
		var rows []corestore.WorkItem
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			project, err := r.Project(ctx, scope.project)
			if err != nil {
				return err
			}
			if project.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			var total int64
			rows, total, err = r.ExportWorkItems(ctx, filter, after, batchLimit)
			if filter.IncludeTotal {
				page.Total = int(total)
			}
			return err
		})
		if err != nil {
			return page, err
		}
		if len(rows) == 0 {
			page.Truncated = false
			break
		}
		accepted := 0
		for _, row := range rows {
			if accepted == batchSize {
				break
			}
			item := string(row.NativeID)
			issue, err := s.readWorkItem(ctx, scope, item)
			if err != nil {
				return page, bulkExportItemFailure(item, "read issue", err)
			}
			exported, err := s.readBulkExportItem(ctx, scope, issue)
			if err != nil {
				var tooLarge *operatortool.ResultTooLargeError
				if errors.As(err, &tooLarge) {
					if len(page.Items) > 0 {
						break
					}
					return page, bulkExportItemFailure(item, "encode item", err)
				}
				return page, bulkExportItemFailure(item, "read comments", err)
			}
			raw, err := json.Marshal(exported)
			if err != nil {
				return page, bulkExportItemFailure(item, "encode item", err)
			}
			if size+len(raw)+1 > operatortool.WorkBulkExportMaxBytes {
				if len(page.Items) == 0 {
					return page, bulkExportItemFailure(item, "encode item", &operatortool.ResultTooLargeError{SizeBytes: size + len(raw), LimitBytes: operatortool.WorkBulkExportMaxBytes})
				}
				break
			}
			size += len(raw) + 1
			page.Items = append(page.Items, exported)
			after = row.Number
			cursor.After = strconv.FormatInt(after, 10)
			accepted++
		}
		page.Truncated = accepted < len(rows)
		if accepted < min(len(rows), batchSize) || !page.Truncated {
			break
		}
	}
	if page.Truncated {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

func (s *Service) readBulkExportItem(ctx context.Context, scope nativeScope, issue tracker.NativeIssue) (operatortool.WorkBulkExportItem, error) {
	exported := operatortool.WorkBulkExportItem{NativeIssue: s.nativeIssueResponse(issue), Comments: []tracker.NativeComment{}}
	raw, err := json.Marshal(exported)
	if err != nil {
		return exported, err
	}
	size, after := len(raw)+8192, int64(0)
	for {
		var rows []corestore.Comment
		var versions []eventstore.CollaborationVersion
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			var err error
			rows, err = r.Comments(ctx, scope.project, issue.WorkItemID, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, row := range rows {
				version, err := r.CollaborationVersion(ctx, row.ID, row.Revision)
				if err != nil {
					return err
				}
				versions = append(versions, version)
			}
			return nil
		})
		if err != nil {
			return exported, err
		}
		for i, row := range rows {
			comment, err := s.hydrateCommentRecord(ctx, row, versions[i])
			if err != nil {
				return exported, err
			}
			raw, err := json.Marshal(comment)
			if err != nil {
				return exported, err
			}
			size += len(raw) + 1
			if size > operatortool.WorkBulkExportMaxBytes {
				return exported, &operatortool.ResultTooLargeError{SizeBytes: size, LimitBytes: operatortool.WorkBulkExportMaxBytes}
			}
			exported.Comments = append(exported.Comments, comment)
			after = row.Sequence
		}
		if len(rows) < corestore.MaxPageSize {
			return exported, nil
		}
	}
}
func bulkExportItemFailure(id, step string, err error) error {
	return &operatortool.WorkBulkExportItemError{Code: "bulk_export_item_failed", Message: "Work item could not be exported: " + step, WorkItemID: id, Err: fmt.Errorf("%s: %w", step, err)}
}
