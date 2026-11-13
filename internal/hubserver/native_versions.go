package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) getNativeVersion(c echo.Context) error {

	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision <= 0 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Revision must be a positive decimal integer"))
	}
	body, err := s.readVersion(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("comment"), revision)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, body)
}

func (s *Service) readVersion(ctx context.Context, scope nativeScope, item, comment string, revision int64) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	recordID := item
	if comment != "" {
		recordID = comment
	}
	var version eventstore.CollaborationVersion
	var archived *attachmentstore.StorageArchiveSection
	var path string
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
		row, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, recordID, revision)
		if errors.Is(err, corestore.ErrNotFound) && revision > 0 {
			owner, ok := r.(attachmentstore.StorageArchiveReader)
			if !ok {
				return err
			}
			section, err := owner.StorageArchiveSection(ctx, attachmentstore.StorageArchiveSectionQuery{ProjectID: scope.project, WorkItemID: row.NativeID, Kind: "revision", Subject: recordID, At: revision})
			if err != nil {
				return err
			}
			if section.OrganizationID != scope.organization || section.Stamp.ProjectID != scope.project || section.Stamp.WorkItemID != row.NativeID || section.Kind != "revision" || section.Subject != recordID || section.First > revision || section.Last < revision || section.Archive.Reference.SchemaVersion != 1 {
				return corestore.ErrNotFound
			}
			archived = &section
		} else {
			if err != nil {
				return err
			}
			if version.OrganizationID != scope.organization || version.ProjectID != scope.project || version.WorkItemID != row.NativeID || version.RecordID != recordID || version.Revision != revision {
				return corestore.ErrNotFound
			}
		}
		if comment == "" {
			organization, err := r.AccessOrganization(ctx)
			if err != nil {
				return err
			}
			path = "/" + organization.Slug + "/" + project.Slug + "/issues/" + strconv.FormatInt(row.Number, 10)
		}
		return nil
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	var body json.RawMessage
	if archived != nil {
		ctx = s.archiveContext(ctx, scope, false)
		state := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead)
		section, found, readErr := state.load(ctx, item, "revision", recordID, revision, archived.ID, archived.First, archived.Last, archived.Archive.Reference)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			return nil, corestore.ErrNotFound
		}
		body, err = archivedVersionPayload(section, recordID, revision)
	} else {
		body, err = eventstore.NewContent(s.runDetailStoreFor(ctx)).CollaborationVersion(ctx, version)
	}
	if err != nil {
		return nil, err
	}
	if comment != "" {
		var value tracker.NativeComment
		if json.Unmarshal(body, &value) != nil || value.OrganizationID != scope.organization || value.ProjectID != scope.project || string(value.WorkItemID) != item || value.ID != comment || int64(value.Revision) != revision {
			return nil, artifact.ErrIntegrity
		}
		return json.RawMessage(body), nil
	}
	var issue tracker.NativeIssue
	if json.Unmarshal(body, &issue) != nil || issue.OrganizationID != scope.organization || issue.ProjectID != scope.project || string(issue.WorkItemID) != item || int64(issue.Revision) != revision {
		return nil, artifact.ErrIntegrity
	}
	if !scope.reporterOwns(issue.Actor) {
		return nil, hubapi.NativeNotFound()
	}
	if scope.credential.Scope != hubapi.APIScopeAdmin || scope.credential.NativeOnly {
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, links := range []*[]tracker.NativeDependency{&issue.Blockers, &issue.Dependents, &issue.Related} {
				kept := []tracker.NativeDependency{}
				for _, link := range *links {
					row, err := r.WorkItemByNativeID(ctx, link.ID)
					if errors.Is(err, corestore.ErrNotFound) {
						continue
					}
					if err != nil {
						return err
					}
					if !scope.reporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
						continue
					}
					relatedScope := scope
					relatedScope.project = row.ProjectID
					if err := s.authorizeAccessScope(ctx, r, relatedScope, false); err != nil {
						if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, corestore.ErrNotFound) || errors.Is(err, auth.ErrHostedIdentity) || hubapi.IsNativeNotFound(err) {
							continue
						}
						return err
					}
					project, err := r.Project(ctx, row.ProjectID)
					if errors.Is(err, corestore.ErrNotFound) {
						continue
					}
					if err != nil {
						return err
					}
					if project.DeletedAt == nil {
						kept = append(kept, link)
					}
				}
				*links = kept
			}
			return nil
		})
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		issue.Dependencies = []tracker.NativeWorkItemID{}
		for _, link := range issue.Blockers {
			issue.Dependencies = append(issue.Dependencies, link.ID)
		}
	}
	issue.BlockedReason.Text, issue.UnblocksReason.Text = "", ""
	issue.WebURL = path
	return json.Marshal(s.nativeIssueResponse(scope.reporterIssue(issue)))
}

func archivedVersionPayload(section attachment.ArchiveSection, recordID string, revision int64) (json.RawMessage, error) {
	for _, table := range section.Tables {
		if table.Name != "collaboration_versions" {
			continue
		}
		names := map[string]bool{}
		for _, name := range table.Columns {
			if names[name] {
				return nil, artifact.ErrIntegrity
			}
			names[name] = true
		}
		idColumn := slices.Index(table.Columns, "record_id")
		revisionColumn := slices.Index(table.Columns, "revision")
		payloadColumn := slices.Index(table.Columns, "record_json")
		encodingColumn := slices.Index(table.Columns, "encoding")
		if idColumn < 0 || revisionColumn < 0 || payloadColumn < 0 || encodingColumn < 0 {
			return nil, artifact.ErrIntegrity
		}
		var result json.RawMessage
		for _, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return nil, artifact.ErrIntegrity
			}
			var id string
			var version int64
			if json.Unmarshal(row[idColumn], &id) != nil || json.Unmarshal(row[revisionColumn], &version) != nil {
				return nil, artifact.ErrIntegrity
			}
			if id != recordID || version != revision {
				continue
			}
			var encoding, payload string
			if result != nil || json.Unmarshal(row[encodingColumn], &encoding) != nil || encoding != "snapshot" || json.Unmarshal(row[payloadColumn], &payload) != nil || !json.Valid([]byte(payload)) {
				return nil, artifact.ErrIntegrity
			}
			result = json.RawMessage(payload)
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, corestore.ErrNotFound
}
