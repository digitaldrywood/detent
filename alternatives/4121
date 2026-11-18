package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readCommentRecords(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.NativeComment], error) {
	var page tracker.Page[tracker.NativeComment]
	err := s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
		var err error
		page, err = s.readCommentPageModel(ctx, scope, item, params)
		return err
	})
	if pageModelPending(err) {
		return s.readCommentSourceRecords(ctx, scope, item, params)
	}
	return page, err
}

func (s *Service) readWorkItemPageSnapshot(ctx context.Context, scope hubscope.NativeScope, run func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if repository, ok := s.config.TenantRepository.(tenantstore.RequestReader); ok {
		return repository.ReadRequest(ctx, scope.Organization, run)
	}
	return run(ctx)
}

func (s *Service) readCommentPageModel(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.NativeComment], error) {
	page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
	if err := hubapi.ValidateNativeQuery(params, "direction"); err != nil {
		return page, err
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(item) + "/comments"
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
			return page, hubapi.NativePublicInvalid("Cursor is invalid")
		}
	}

	var records []viewstore.CommentPageRecord
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(viewstore.WorkItemPageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		records, err = reader.CommentPageRecords(ctx, scope.Project, tracker.NativeWorkItemID(item), after, newest, limit)
		return err
	})
	if err != nil {
		return page, pageModelError(err)
	}
	more := len(records) > limit

	size := 8192
	for _, record := range records[:min(limit, len(records))] {
		comment := record.Value
		raw, err := json.Marshal(comment)
		if err != nil {
			return page, err
		}
		if size+len(raw)+1 > operatortool.MaxResultBytes {
			if len(page.Items) == 0 {
				return page, hubapi.NativePublicInvalid("A single comment exceeds the page budget; use " + operatortool.WorkBulkExport + " to read the full comment")
			}
			more = true
			break
		}
		size += len(raw) + 1
		page.Items = append(page.Items, comment)
		cursor.After = strconv.FormatInt(comment.Sequence, 10)
	}
	if more {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
	}
	return page, err
}

func (s *Service) authorizeCommentWorkItem(ctx context.Context, scope hubscope.NativeScope, item string) error {
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		return nil
	})
	return hubapi.ProjectRecordError(err)
}

func (s *Service) workItemResource(ctx context.Context, scope hubscope.NativeScope, id string, params url.Values) (tracker.NativeIssue, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return tracker.NativeIssue{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	for name, values := range params {
		if name != "include" || len(values) != 1 {
			return tracker.NativeIssue{}, hubapi.NativePublicInvalid("Unsupported work item query")
		}
	}
	parts := strings.Split(params.Get("include"), ",")
	workpad := slices.Contains(parts, "workpad")
	parts = slices.DeleteFunc(parts, func(value string) bool { return value == "workpad" })
	include, err := nativeIssueChangeIncluded(strings.Join(parts, ","))
	if err != nil {
		return tracker.NativeIssue{}, err
	}

	var issue tracker.NativeIssue
	var source eventstore.LinkedSource
	project, err := s.readTenantProjectRecord(ctx, scope)
	if err != nil {
		return issue, err
	}
	if project.Core.Profile == "github_compatible" {
		issue, err = s.readWorkItemSourcePage(ctx, scope, id)
	} else {
		err = s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
			var err error
			issue, source, err = s.readWorkItemPageModel(ctx, scope, id)
			return err
		})
		if err == nil {
			issue, err = s.workItemReader().LinkSource(ctx, scope.Organization, issue, source)
		}
	}
	if pageModelPending(err) {
		issue, err = s.readWorkItemSourcePage(ctx, scope, id)
	}
	if err == nil {
		issue.Queue, issue.Workpad = nil, nil
		issue.DependencyReady = false
		issue.SchedulingEligible = nil
		for _, field := range []string{"dependency_ready", "scheduling_eligible"} {
			if !slices.Contains(issue.OmittedFields, field) {
				issue.OmittedFields = append(issue.OmittedFields, field)
			}
		}
	}
	if err == nil && workpad {
		err = s.hydrateWorkItemWorkpad(ctx, scope, &issue)
	}
	if err == nil && include {
		err = s.readNativeIssueChangePage(ctx, scope, &issue)
	}
	return issue, err
}

func (s *Service) readWorkItemPageModel(ctx context.Context, scope hubscope.NativeScope, id string) (tracker.NativeIssue, eventstore.LinkedSource, error) {
	var source eventstore.LinkedSource
	if err := s.authorizeCommentWorkItem(ctx, scope, id); err != nil {
		return tracker.NativeIssue{}, source, err
	}
	var record viewstore.ItemPageRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(viewstore.WorkItemPageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := requireWorkItemPageReady(ctx, r, scope, id, "item"); err != nil {
			return err
		}
		var err error
		record, err = reader.WorkItemDetailPage(ctx, scope.Project, tracker.NativeWorkItemID(id))
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err != nil {
			return err
		}
		source, err = r.LinkedSource(ctx, scope.Project, tracker.NativeWorkItemID(id))
		if errors.Is(err, corestore.ErrNotFound) {
			source, err = eventstore.LinkedSource{}, nil
		}
		return err
	})
	if err == nil {
		err = s.workItemReader().Complete(ctx, scope, &record.Value)
	}
	return record.Value, source, hubapi.ProjectRecordError(err)
}

func (s *Service) GetWorkItemReferences(c echo.Context) error {
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(params) != 0 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("References do not accept query parameters"))
	}
	scope := hubscope.NativeRequestScope(c)
	var references []tracker.ExternalReference
	err = s.readWorkItemPageSnapshot(c.Request().Context(), scope, func(ctx context.Context) error {
		if err := s.authorizeCommentWorkItem(ctx, scope, c.Param("item")); err != nil {
			return err
		}
		return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(viewstore.WorkItemReferenceReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			var err error
			if err := requireWorkItemPageReady(ctx, r, scope, c.Param("item"), "item"); err != nil {
				return err
			}
			references, err = reader.WorkItemReferenceRecords(ctx, scope.Project, tracker.NativeWorkItemID(c.Param("item")))
			return pageModelError(err)
		})
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, references)
}

func (s *Service) readCommentSourceRecords(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.NativeComment], error) {
	page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
	if err := hubapi.ValidateNativeQuery(params, "direction"); err != nil {
		return page, err
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(item) + "/comments"
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
			return page, hubapi.NativePublicInvalid("Cursor is invalid")
		}
	}
	var rows []corestore.Comment
	var versions []eventstore.CollaborationVersion
	more := false
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.SelectComments(ctx, scope.Project, tracker.NativeWorkItemID(item), after, newest, limit)
		if err != nil {
			return err
		}
		if len(rows) == limit {
			boundary, err := r.SelectComments(ctx, scope.Project, tracker.NativeWorkItemID(item), rows[len(rows)-1].Sequence, newest, 1)
			if err != nil {
				return err
			}
			more = len(boundary) > 0
		}

		batch, ok := r.(eventstore.CollaborationVersionsReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
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
		return page, hubapi.ProjectRecordError(err)
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
				return page, hubapi.NativePublicInvalid("A single comment exceeds the page budget; use " + operatortool.WorkBulkExport + " to read the full comment")
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

func pageModelPending(err error) bool {
	var public *hubapi.NativeError
	return errors.As(err, &public) && public.Code == "not_ready"
}
func pageModelError(err error) error {
	if errors.Is(err, viewstore.ErrPagePending) || errors.Is(err, corestore.ErrNotFound) {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return hubapi.ProjectRecordError(err)
}
func requireWorkItemPageReady(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item, kind string) error {
	reader, ok := r.(viewstore.WorkItemPageReadinessReader)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	ready, err := reader.WorkItemPageReady(ctx, scope.Project, tracker.NativeWorkItemID(item), kind)
	if err != nil {
		return err
	}
	if !ready {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return nil
}

func (s *Service) readWorkItemSourcePage(ctx context.Context, scope hubscope.NativeScope, id string) (tracker.NativeIssue, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.RequestReader)
	if !ok {
		return tracker.NativeIssue{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	var source eventstore.LinkedSource
	err := repository.ReadRequest(ctx, scope.Organization, func(ctx context.Context) error {
		return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			var err error
			row, version, err = s.readWorkItemRecord(ctx, scope, id)
			if err != nil {
				return err
			}
			source, err = r.LinkedSource(ctx, scope.Project, row.NativeID)
			if errors.Is(err, corestore.ErrNotFound) {
				source = eventstore.LinkedSource{}
				return nil
			}
			return err
		})
	})
	if err != nil {
		return tracker.NativeIssue{}, hubapi.ProjectRecordError(err)
	}
	issue, err := s.hydrateCanonicalWorkItemRecord(ctx, row, version)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if source.OrganizationID != "" {
		linked, err := s.hydrateLinkedIssueSourceRecord(ctx, scope.Organization, source)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		issue.LinkedSource = &linked
	}
	err = repository.ReadRequest(ctx, scope.Organization, func(ctx context.Context) error {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
			current, err := r.WorkItem(ctx, scope.Project, row.NativeID)
			if err != nil {
				return err
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
				return corestore.ErrNotFound
			}
			if current.Revision != row.Revision {
				return hubapi.NativeConflict(tracker.Revision(current.Revision))
			}
			currentVersion, err := r.CollaborationVersion(ctx, id, current.Revision)
			if err != nil {
				return err
			}
			if currentVersion.RecordID != version.RecordID || currentVersion.PayloadObjectKey != version.PayloadObjectKey || currentVersion.PayloadSHA256 != version.PayloadSHA256 {
				return hubapi.NativeConflict(tracker.Revision(current.Revision))
			}
			currentSource, err := r.LinkedSource(ctx, scope.Project, row.NativeID)
			if errors.Is(err, corestore.ErrNotFound) {
				currentSource, err = eventstore.LinkedSource{}, nil
			}
			if err != nil {
				return err
			}
			if currentSource.SnapshotObjectKey != source.SnapshotObjectKey || currentSource.SnapshotSHA256 != source.SnapshotSHA256 {
				return hubapi.NativeConflict(tracker.Revision(current.Revision))
			}
			issue.Number, issue.Revision = int(current.Number), tracker.Revision(current.Revision)
			issue.Title, issue.State, issue.Archived = current.Title, hubapi.StringValue(current.WorkflowStateName), current.Archived
			issue.Type, issue.Priority = current.IssueType, current.Priority
			issue.Actor.Kind, issue.Actor.PrincipalID = hubapi.StringValue(current.ActorKind), hubapi.StringValue(current.ActorPrincipalID)
			issue.LastActivityAt = current.LastActivityAt
			if currentSource.OrganizationID == "" {
				issue.LinkedSource = nil
			} else {
				if issue.LinkedSource == nil {
					issue.LinkedSource = &tracker.LinkedIssueSource{Status: "pending"}
				}
				if issue.LinkedSource.Snapshot != nil && issue.LinkedSource.Snapshot.URL != currentSource.URL {
					return artifact.ErrIntegrity
				}
				issue.LinkedSource.URL = currentSource.URL
				if reference := tracker.GitHubIssueSourceReference(currentSource.URL, currentSource.URL); !slices.Contains(issue.ExternalReferences, reference) {
					issue.ExternalReferences = append(issue.ExternalReferences, reference)
				}
			}
			organization, err := r.AccessOrganization(ctx)
			if err != nil {
				return err
			}
			issue.WebURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(organization.Slug, project.Slug, issue.Number)}).WebURL
			return nil
		}); err != nil {
			return err
		}
		return s.workItemReader().Complete(ctx, scope, &issue)
	})
	return issue, hubapi.ProjectRecordError(err)
}

func (s *Service) hydrateWorkItemWorkpad(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	var outbound projectstore.GitHubOutbound
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		if item.Revision != int64(issue.Revision) {
			return corestore.ErrRevisionConflict
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		owner, ok := r.(projectstore.GitHubWorkpadReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		outbound, err = owner.LatestGitHubWorkpad(ctx, scope.Project, issue.WorkItemID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	if outbound.ID == 0 {
		return nil
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, outbound.Payload)
	if err != nil {
		return err
	}
	var desired WorkpadDesired
	if err := json.Unmarshal(raw, &desired); err != nil {
		return err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if item.Revision != int64(issue.Revision) {
			return corestore.ErrRevisionConflict
		}
		return nil
	}); err != nil {
		return hubapi.ProjectRecordError(err)
	}
	issue.Workpad = &tracker.NativeWorkpad{Phase: desired.Phase, Body: desired.Body, SyncState: outbound.Status, UpdatedAt: outbound.CreatedAt}
	return nil
}
