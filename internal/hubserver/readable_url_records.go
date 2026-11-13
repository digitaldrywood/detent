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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readableRecordReference(ctx context.Context, scope hubscope.NativeScope, parts []string, authorize bool) (string, error) {
	if len(parts) < 4 || parts[2] != "issues" || scope.Organization != tracker.OrganizationID(s.appOrganizationID()) {
		return "", hubapi.NativeNotFound()
	}
	number, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || number < 1 {
		return "", hubapi.NativeNotFound()
	}
	var id string
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		if project.Core.DeletedAt != nil || scope.Project != "" && scope.Project != project.Core.ID {
			return corestore.ErrNotFound
		}
		scope.Project = project.Core.ID
		if authorize {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
		}
		row, err := r.WorkItemByNumber(ctx, scope.Project, number)
		if err != nil {
			return err
		}
		if authorize && !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		id = string(row.NativeID)
		return nil
	})
	return id, hubapi.ProjectRecordError(err)
}

func (s *Service) organizationSlugRecordSettings(c echo.Context, scope hubscope.NativeScope) error {
	ctx := c.Request().Context()
	if s.config.AccessRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	if err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, false) }); err != nil {
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
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Edit the organization slug through the shared entry"))
	}
	scope.RequireHostedAdmin = true
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	var input struct {
		Slug         string `json:"slug"`
		ExpectedSlug string `json:"expected_slug"`
	}
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := readableurl.Validate(input.Slug); err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		return w.SetAccessOrganizationSlug(ctx, &input.ExpectedSlug, input.Slug)
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = hubapi.NativePublicInvalid("The slug changed; reload and try again")
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"slug": input.Slug})
}

func (s *Service) readableLegacyRecordPath(ctx context.Context, credential hubapi.APICredential, parts []string, kind string) (string, error) {
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID()), Credential: credential}
	var item corestore.WorkItem
	var projectSlug string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		switch kind {
		case "issue":
			item, err = r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(parts[2]))
			scope.Project = item.ProjectID
		case "project_issue":
			scope.Project = tracker.ProjectID(parts[1])
			if number, parseErr := strconv.ParseInt(parts[3], 10, 64); parseErr == nil {
				item, err = r.WorkItemByNumber(ctx, scope.Project, number)
			} else {
				item, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(parts[3]))
			}
		case "project":
			scope.Project = tracker.ProjectID(parts[2])
		}
		if err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if kind != "project" && !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		projectSlug = project.Slug
		return nil
	})
	if err != nil {
		return "", hubapi.ProjectRecordError(err)
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
		if s.config.Hosted != nil {
			path = strings.TrimRight(s.config.Hosted.PublicURL, "/") + path
		}
	}
	return path, nil
}

func (s *Service) readableOrganizationSlug(ctx context.Context) (string, error) {
	var slug string
	if s.config.AccessRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	err := s.accessRead(ctx, tracker.OrganizationID(s.appOrganizationID()), func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(ctx)
		slug = organization.Slug
		return err
	})
	return slug, err
}

func (s *Service) readablePageRecord(ctx context.Context, credential hubapi.APICredential, parts []string) (string, string, string, error) {
	if s.config.TenantRepository == nil {
		return "", "", "", hubapi.NativeWorkItemStorageUnavailable()
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID()), Credential: credential}
	var projectSlug, item string
	var number int64
	if len(parts) > 1 {
		parsed, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || parsed < 1 {
			return "", "", "", hubapi.NativeNotFound()
		}
		number = parsed
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.ProjectBySlug(ctx, parts[0])
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		scope.Project = project.Core.ID
		projectSlug = project.Core.Slug
		return nil
	})
	if err != nil {
		return "", "", "", hubapi.ProjectRecordError(err)
	}
	if s.config.AccessRepository == nil {
		return "", "", "", hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, false) }); err != nil {
		return "", "", "", err
	}
	orgSlug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return "", "", "", err
	}
	canonical := "/" + orgSlug + "/" + projectSlug
	if len(parts) == 1 {
		return string(scope.Project), "", canonical, nil
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItemByNumber(ctx, scope.Project, number)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		item = string(row.NativeID)
		if len(parts) == 5 {
			change, err := r.Change(ctx, parts[4])
			if err != nil {
				return err
			}
			if change.ProjectID != scope.Project {
				return corestore.ErrNotFound
			}
			links, err := r.Links(ctx, change.ID, "", 33)
			if err != nil {
				return err
			}
			for _, link := range links {
				if link.WorkItemID == row.NativeID && link.ProjectID == scope.Project {
					return nil
				}
			}
			return corestore.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return "", "", "", hubapi.ProjectRecordError(err)
	}
	canonical = "/" + orgSlug + "/" + projectSlug + "/issues/" + strconv.FormatInt(number, 10)
	if len(parts) == 5 {
		canonical += "/changes/" + url.PathEscape(parts[4])
	}
	return string(scope.Project), item, canonical, nil
}

func (s *Service) readIssueBrowserPath(ctx context.Context, organization tracker.OrganizationID, id string) (string, error) {
	if s.config.TenantRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	var organizationSlug, projectSlug string
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
		tenant, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		organizationSlug = tenant.Slug
		projectSlug, number = project.Slug, row.Number
		return nil
	})
	if err != nil {
		return "", hubapi.ProjectRecordError(err)
	}
	return "/" + organizationSlug + "/" + projectSlug + "/issues/" + strconv.FormatInt(number, 10), nil
}

func (s *Service) projectBrowserPath(ctx context.Context, id tracker.ProjectID) (string, error) {
	if s.config.TenantRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
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
		return "", hubapi.ProjectRecordError(err)
	}
	orgSlug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return "", err
	}
	return "/" + orgSlug + "/" + projectSlug, nil
}
