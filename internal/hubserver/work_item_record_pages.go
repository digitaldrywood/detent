package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readCommentRecords(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.NativeComment], error) {
	page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
	if err := validateNativeQuery(params, "direction"); err != nil {
		return page, err
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/comments"
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return page, err
	}
	if err := s.authorizeCommentWorkItem(ctx, scope, item); err != nil {
		return page, err
	}
	if _, _, err := nativePageOrder(params, "sequence", cursor.After); err != nil {
		return page, err
	}
	newest := params.Get("direction") == "newest"
	after := int64(0)
	if newest {
		after = math.MaxInt64
	}
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil || after < 0 {
			return page, nativePublicInvalid("Cursor is invalid")
		}
	}
	var rows []corestore.Comment
	var versions []eventstore.CollaborationVersion
	more := false
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.SelectComments(ctx, scope.project, tracker.NativeWorkItemID(item), after, newest, limit)
		if err != nil {
			return err
		}
		if len(rows) == limit {
			boundary, err := r.SelectComments(ctx, scope.project, tracker.NativeWorkItemID(item), rows[len(rows)-1].Sequence, newest, 1)
			if err != nil {
				return err
			}
			more = len(boundary) > 0
		}

		batch, ok := r.(eventstore.CollaborationVersionsReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		keys := make([]eventstore.CollaborationVersionKey, len(rows))
		for i, row := range rows {
			keys[i] = eventstore.CollaborationVersionKey{RecordID: row.ID, Revision: row.Revision}
		}
		found, err := batch.CollaborationVersions(ctx, keys)
		if err != nil {
			return err
		}
		byKey := make(map[eventstore.CollaborationVersionKey]eventstore.CollaborationVersion, len(found))
		for _, version := range found {
			byKey[eventstore.CollaborationVersionKey{RecordID: version.RecordID, Revision: version.Revision}] = version
		}
		for _, key := range keys {
			version, ok := byKey[key]
			if !ok {
				return corestore.ErrNotFound
			}
			versions = append(versions, version)
		}
		return nil
	})
	if err != nil {
		return page, projectRecordError(err)
	}
	size := 8192
	for i, row := range rows[:min(limit, len(rows))] {
		comment, err := s.hydrateCommentRecord(ctx, row, versions[i])
		if err != nil {
			return page, err
		}
		raw, err := json.Marshal(comment)
		if err != nil {
			return page, err
		}
		if size+len(raw)+1 > operatortool.MaxResultBytes {
			if len(page.Items) == 0 {
				return page, nativePublicInvalid("A single comment exceeds the page budget")
			}
			more = true
			break
		}
		size += len(raw) + 1
		page.Items = append(page.Items, comment)
		cursor.After = strconv.FormatInt(row.Sequence, 10)
	}
	if more {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
	}
	return page, err
}

func (s *Service) authorizeCommentWorkItem(ctx context.Context, scope nativeScope, item string) error {
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		return nil
	})
	return projectRecordError(err)
}

func (s *Service) workItemResource(ctx context.Context, scope nativeScope, id string, params url.Values) (tracker.NativeIssue, error) {
	if len(params) > 0 {
		for name, values := range params {
			if name != "include" || len(values) != 1 || strings.TrimSpace(values[0]) != "" {
				return tracker.NativeIssue{}, workItemStorageUnavailable()
			}
		}
	}

	var issue tracker.NativeIssue
	err := s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
		var err error
		issue, err = s.readWorkItemPageModel(ctx, scope, id)
		return err
	})
	return issue, err
}

func (s *Service) readWorkItemPageSnapshot(ctx context.Context, scope nativeScope, run func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if repository, ok := s.config.TenantRepository.(tenantstore.RequestReader); ok {
		return repository.ReadRequest(ctx, scope.organization, run)
	}
	return run(ctx)
}

func (s *Service) readWorkItemPageModel(ctx context.Context, scope nativeScope, id string) (tracker.NativeIssue, error) {

	if err := s.authorizeCommentWorkItem(ctx, scope, id); err != nil {
		return tracker.NativeIssue{}, err
	}
	var record viewstore.ItemPageRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(viewstore.WorkItemPageReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		record, err = reader.WorkItemDetailPage(ctx, scope.project, tracker.NativeWorkItemID(id))
		if errors.Is(err, corestore.ErrNotFound) {
			return workItemStorageUnavailable()
		}
		return err
	})
	if err != nil {
		return tracker.NativeIssue{}, projectRecordError(err)
	}
	return scope.reporterIssue(record.Value), nil
}

func (s *Service) GetWorkItemReferences(c echo.Context) error {
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(params) != 0 {
		return s.nativeAPIError(c, nativePublicInvalid("References do not accept query parameters"))
	}
	scope := nativeRequestScope(c)
	var references []tracker.ExternalReference
	err = s.readWorkItemPageSnapshot(c.Request().Context(), scope, func(ctx context.Context) error {
		if err := s.authorizeCommentWorkItem(ctx, scope, c.Param("item")); err != nil {
			return err
		}
		return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(viewstore.WorkItemReferenceReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			var err error
			references, err = reader.WorkItemReferenceRecords(ctx, scope.project, tracker.NativeWorkItemID(c.Param("item")))
			return err
		})
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, references)
}

func WorkItemPageOrganizationRoute(path string) bool {
	switch path {
	case nativeOrganizationIssuePath, nativeOrganizationIssuePath + "/comments", nativeOrganizationIssuePath + "/relationships", nativeOrganizationIssuePath + "/references":
		return true
	default:
		return false
	}
}
