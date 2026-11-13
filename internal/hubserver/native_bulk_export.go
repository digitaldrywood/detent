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

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) exportNativeWorkItems(c echo.Context) error {
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page, err := s.readBulkExport(c.Request().Context(), hubscope.NativeRequestScope(c), params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readBulkExport(ctx context.Context, scope hubscope.NativeScope, params url.Values) (operatortool.WorkBulkExportPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	page := operatortool.WorkBulkExportPage{ProjectID: string(scope.Project), GeneratedAt: s.config.Clock(), Freshness: explain.SourceAvailable, Items: []operatortool.WorkBulkExportItem{}}
	single := url.Values{}
	for name, values := range params {
		single[name] = values
	}
	single.Del("state")
	single.Del("label")
	if err := hubapi.ValidateNativeQuery(single, "open", "archived"); err != nil {
		return page, err
	}
	if err := hubrecords.ValidateNativeIssueQuery(params); err != nil {
		return page, err
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/export"
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
			return page, hubapi.NativePublicInvalid("Cursor is invalid")
		}
	}
	filter := corestore.WorkItemExportSelection{ProjectID: scope.Project, States: selection.States, Labels: selection.Labels, Archived: selection.Archived, Terminal: selection.Terminal, Reporter: selection.Reporter, PrincipalID: scope.Actor().PrincipalID}
	size := 8192
	for len(page.Items) < limit {
		filter.IncludeTotal = len(page.Items) == 0
		batchSize := min(corestore.MaxPageSize-1, limit-len(page.Items))
		batchLimit := batchSize + 1
		var rows []corestore.WorkItem
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			project, err := r.Project(ctx, scope.Project)
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
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeBulkExportItem(ctx, r, scope, ""); err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := s.authorizeBulkExportItem(ctx, r, scope, item.WorkItemID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return page, err
	}
	if page.Truncated {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

func (s *Service) readBulkExportItem(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue) (operatortool.WorkBulkExportItem, error) {
	exported := operatortool.WorkBulkExportItem{NativeIssue: s.nativeIssueResponse(issue), Comments: []tracker.NativeComment{}}
	raw, err := json.Marshal(exported)
	if err != nil {
		return exported, err
	}
	size, after := len(raw)+8192, int64(0)
	for {
		var rows []corestore.Comment
		var versions []eventstore.CollaborationVersion
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeBulkExportItem(ctx, r, scope, issue.WorkItemID); err != nil {
				return err
			}
			var err error
			rows, err = r.Comments(ctx, scope.Project, issue.WorkItemID, after, corestore.MaxPageSize)
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
			err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { return s.authorizeBulkExportItem(ctx, r, scope, issue.WorkItemID) })
			return exported, err
		}
	}
}
func (s *Service) authorizeBulkExportItem(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID) error {
	if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
		return err
	}
	if scope.Credential.Hosted != nil {
		member, err := r.AuthorizeAccess(ctx, s.accessAuthority(scope, false), s.config.Clock())
		if err != nil {
			return err
		}
		if member.UserID != "" && hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole) != scope.Credential.HostedRole {
			return auth.ErrHostedIdentity
		}
	}
	project, err := r.Project(ctx, scope.Project)
	if err != nil {
		return err
	}
	if project.DeletedAt != nil {
		return corestore.ErrNotFound
	}
	if item == "" {
		return nil
	}
	row, err := r.WorkItem(ctx, scope.Project, item)
	if err != nil {
		return err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
		return corestore.ErrNotFound
	}
	return nil
}

func bulkExportItemFailure(id, step string, err error) error {
	return &operatortool.WorkBulkExportItemError{Code: "bulk_export_item_failed", Message: "Work item could not be exported: " + step, WorkItemID: id, Err: fmt.Errorf("%s: %w", step, err)}
}
