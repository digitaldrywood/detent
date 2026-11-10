package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readableRecordReference(ctx context.Context, scope nativeScope, parts []string, authorize bool) (string, error) {
	if len(parts) < 4 || parts[2] != "issues" || scope.organization != tracker.OrganizationID(s.appOrganizationID()) {
		return "", nativeNotFound()
	}
	number, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || number < 1 {
		return "", nativeNotFound()
	}
	var id string
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owned, err := r.AccessOrganizationSlug(ctx, parts[0])
		if err != nil {
			return err
		}
		if !owned {
			return corestore.ErrNotFound
		}
		project, err := r.ProjectBySlug(ctx, parts[1])
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || scope.project != "" && scope.project != project.Core.ID {
			return corestore.ErrNotFound
		}
		scope.project = project.Core.ID
		if authorize {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
		}
		row, err := r.WorkItemByNumber(ctx, scope.project, number)
		if err != nil {
			return err
		}
		if authorize && !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		id = string(row.NativeID)
		return nil
	})
	return id, projectRecordError(err)
}

func (s *Service) organizationSlugRecordSettings(c echo.Context, scope nativeScope) error {
	ctx := c.Request().Context()
	if s.config.AccessRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	if err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, false) }); err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Request().Method == http.MethodGet {
		slug, err := s.readableOrganizationSlug(ctx)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, map[string]string{"slug": slug})
	}
	if s.hostedShared() {
		return s.nativeAPIError(c, nativePublicInvalid("Edit the organization slug through the shared entry"))
	}
	scope.requireHostedAdmin = true
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	var input struct {
		Slug         string `json:"slug"`
		ExpectedSlug string `json:"expected_slug"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := readableurl.Validate(input.Slug); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		return w.SetAccessOrganizationSlug(ctx, &input.ExpectedSlug, input.Slug)
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = nativePublicInvalid("The slug changed; reload and try again")
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"slug": input.Slug})
}

func (s *Service) readableLegacyRecordPath(ctx context.Context, credential apiCredential, parts []string, kind string) (string, error) {
	scope := nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential}
	var item corestore.WorkItem
	var projectSlug string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		switch kind {
		case "issue":
			item, err = r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(parts[2]))
			scope.project = item.ProjectID
		case "project_issue":
			scope.project = tracker.ProjectID(parts[1])
			if number, parseErr := strconv.ParseInt(parts[3], 10, 64); parseErr == nil {
				item, err = r.WorkItemByNumber(ctx, scope.project, number)
			} else {
				item, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(parts[3]))
			}
		case "project":
			scope.project = tracker.ProjectID(parts[2])
		}
		if err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if kind != "project" && !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		projectSlug = project.Slug
		return nil
	})
	if err != nil {
		return "", projectRecordError(err)
	}
	orgSlug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return "", err
	}
	path := "/" + orgSlug + "/" + projectSlug
	if kind != "project" {
		path = issueBrowserPath(orgSlug, projectSlug, int(item.Number))
		start := 3
		if kind == "project_issue" {
			start = 4
		}
		if len(parts) > start {
			path += "/" + strings.Join(parts[start:], "/")
		}
	}
	return path, nil
}

func (s *Service) readableOrganizationSlug(ctx context.Context) (string, error) {
	var slug string
	if s.config.AccessRepository == nil {
		return "", workItemStorageUnavailable()
	}
	err := s.accessRead(ctx, tracker.OrganizationID(s.appOrganizationID()), func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(ctx)
		slug = organization.Slug
		return err
	})
	return slug, err
}

func (s *Service) readablePageRecord(ctx context.Context, credential apiCredential, parts []string) (string, string, string, error) {
	if s.config.TenantRepository == nil {
		return "", "", "", workItemStorageUnavailable()
	}
	scope := nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential}
	var projectSlug, item string
	var number int64
	if len(parts) > 1 {
		parsed, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || parsed < 1 {
			return "", "", "", nativeNotFound()
		}
		number = parsed
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.ProjectBySlug(ctx, parts[0])
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		scope.project = project.Core.ID
		projectSlug = project.Core.Slug
		return nil
	})
	if err != nil {
		return "", "", "", projectRecordError(err)
	}
	if s.config.AccessRepository == nil {
		return "", "", "", workItemStorageUnavailable()
	}
	if err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, false) }); err != nil {
		return "", "", "", err
	}
	orgSlug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return "", "", "", err
	}
	canonical := "/" + orgSlug + "/" + projectSlug
	if len(parts) == 1 {
		return string(scope.project), "", canonical, nil
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItemByNumber(ctx, scope.project, number)
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		item = string(row.NativeID)
		if len(parts) == 5 {
			change, err := r.Change(ctx, parts[4])
			if err != nil {
				return err
			}
			if change.ProjectID != scope.project {
				return corestore.ErrNotFound
			}
			links, err := r.Links(ctx, change.ID, "", 33)
			if err != nil {
				return err
			}
			for _, link := range links {
				if link.WorkItemID == row.NativeID && link.ProjectID == scope.project {
					return nil
				}
			}
			return corestore.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return "", "", "", projectRecordError(err)
	}
	canonical = "/" + orgSlug + "/" + projectSlug + "/issues/" + strconv.FormatInt(number, 10)
	if len(parts) == 5 {
		canonical += "/changes/" + url.PathEscape(parts[4])
	}
	return string(scope.project), item, canonical, nil
}

func (s *Service) readIssueBrowserPath(ctx context.Context, id string) (string, error) {
	if s.config.TenantRepository == nil {
		return "", workItemStorageUnavailable()
	}
	organization := tracker.OrganizationID(s.appOrganizationID())
	var projectSlug string
	var number int64
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		project, err := r.Project(ctx, row.ProjectID)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		projectSlug, number = project.Slug, row.Number
		return nil
	})
	if err != nil {
		return "", projectRecordError(err)
	}
	slug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return "", err
	}
	return "/" + slug + "/" + projectSlug + "/issues/" + strconv.FormatInt(number, 10), nil
}

func (s *Service) projectBrowserPath(ctx context.Context, id tracker.ProjectID) (string, error) {
	if s.config.TenantRepository == nil {
		return "", workItemStorageUnavailable()
	}
	var projectSlug string
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, id)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		projectSlug = project.Slug
		return nil
	})
	if err != nil {
		return "", projectRecordError(err)
	}
	orgSlug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return "", err
	}
	return "/" + orgSlug + "/" + projectSlug, nil
}
