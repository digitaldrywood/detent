package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readIssueBrowserPath(ctx context.Context, query hubapi.NativeQueryer, id string) (string, error) {
	var organization, project string
	var number int
	if err := query.QueryRowContext(ctx, `SELECT o.slug,p.slug,i.number FROM issues i JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id WHERE i.native_id=?`, id).Scan(&organization, &project, &number); err != nil {
		return "", err
	}
	return hubrecords.IssueBrowserPath(organization, project, number), nil
}

func (s *Service) issueBrowserURL(ctx context.Context, organization tracker.OrganizationID, id string) (string, error) {
	path, err := s.readIssueBrowserPath(ctx, organization, id)
	if err != nil {
		return "", err
	}
	if s.config.Hosted != nil {
		path = strings.TrimRight(s.config.Hosted.PublicURL, "/") + path
	}
	return path, nil
}

func (s *Service) syncOrganizationSlug(ctx context.Context, slug string) error {
	if slug == "" {
		return nil
	}
	repository := s.config.TenantRepository
	if repository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	current, err := s.readableOrganizationSlug(ctx)
	if err != nil || current == slug {
		return err
	}
	return repository.WriteTenant(ctx, tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(w tenantstore.Writer) error { return w.SetAccessOrganizationSlug(ctx, nil, slug) })
}

func (s *Service) slugSettings(c echo.Context) error {
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	project := c.Param("project")
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID()), Project: tracker.ProjectID(project), Credential: credential}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	if project != "" {
		return s.projectRecordSlugSettings(c, scope)
	}
	return s.organizationSlugRecordSettings(c, scope)
}

func (s *Service) readableReference(ctx context.Context, scope hubscope.NativeScope, reference string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", hubapi.NativeNotFound()
	}
	if parsed.IsAbs() {
		if s.config.Hosted == nil {
			return "", hubapi.NativeNotFound()
		}
		public, err := url.Parse(s.config.Hosted.PublicURL)
		if err != nil || parsed.Scheme != public.Scheme || parsed.Host != public.Host {
			return "", hubapi.NativeNotFound()
		}
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return "", hubapi.NativeNotFound()
	}
	if s.config.TenantRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.readableRecordReference(ctx, scope, parts, true)
}

func (s *Service) canonicalToolResult(ctx context.Context, result operatortool.Result, name string) (operatortool.Result, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(result.Content))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return result, err
	}
	var visit func(any) error
	visit = func(value any) error {
		switch value := value.(type) {
		case []any:
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, child := range value {
				if link, ok := child.(string); ok && (key == "url" || key == "href" || strings.HasSuffix(key, "_url")) {
					if name == operatortool.WorkList {
						if key == "web_url" || key == "url" && value["web_url"] == link {
							continue
						}
					}
					parsed, err := url.Parse(link)
					if err != nil {
						continue
					}
					if strings.HasPrefix(parsed.Path, "/api/") && key != "url" && key != "web_url" && key != "browser_url" && key != "issue_url" {
						continue
					}
					canonical, err := s.canonicalBrowserLink(ctx, link)
					if err != nil {
						return err
					}
					value[key] = canonical
				} else if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(value); err != nil {
		return operatortool.Result{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return operatortool.Result{}, err
	}
	limit := operatortool.ResultByteLimit(name)
	if len(raw) > limit {
		return operatortool.Result{}, &operatortool.ResultTooLargeError{SizeBytes: len(raw), LimitBytes: limit}
	}
	return operatortool.Result{Content: raw}, nil
}

func (s *Service) canonicalBrowserLink(ctx context.Context, link string) (string, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return link, nil
	}
	if parsed.Host != "" {
		if s.config.Hosted == nil {
			return link, nil
		}
		public, err := url.Parse(s.config.Hosted.PublicURL)
		if err != nil {
			return "", err
		}
		if parsed.Host != public.Host || parsed.Scheme != public.Scheme {
			return link, nil
		}
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	path := ""
	for i, part := range parts {
		if strings.HasPrefix(part, "wi_") && i > 0 && (parts[i-1] == "i" || parts[i-1] == "issues" || parts[i-1] == "work-items") {
			path, err = s.readIssueBrowserPath(ctx, tracker.OrganizationID(s.appOrganizationID()), part)
			if err != nil {
				return "", err
			}
			if i+1 < len(parts) {
				path += "/" + strings.Join(parts[i+1:], "/")
			}
			break
		}
	}
	if path == "" && len(parts) >= 4 && parts[2] == "issues" {
		if s.config.TenantRepository == nil {
			return "", hubapi.NativeWorkItemStorageUnavailable()
		}
		id, err := s.readableRecordReference(ctx, hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID())}, parts, false)
		if err == nil {
			path, err = s.readIssueBrowserPath(ctx, tracker.OrganizationID(s.appOrganizationID()), id)
			if err != nil {
				return "", err
			}
			if len(parts) > 4 {
				path += "/" + strings.Join(parts[4:], "/")
			}
		} else if !hubapi.IsNativeNotFound(err) {
			return "", err
		}
	}
	if path == "" {
		for i, part := range parts {
			if strings.HasPrefix(part, "prj_") && i > 0 && (parts[i-1] == "projects" || parts[i-1] == "p") && i == len(parts)-1 {
				path, err = s.projectBrowserPath(ctx, tracker.ProjectID(part))
				if err != nil {
					return "", err
				}
				break
			}
		}
	}
	if path == "" && s.config.Hosted != nil {
		localPath := strings.TrimPrefix(parsed.Path, s.hostedBase())
		first := strings.Split(strings.Trim(localPath, "/"), "/")[0]
		if localPath == "" || localPath == "/" {
			localPath = "/work"
			first = "work"
		}
		if slices.Contains([]string{"work", "chat", "settings", "projects", "organization", "fleet", "reports", "diagnostics", "support"}, first) {
			orgSlug, err := s.readableOrganizationSlug(ctx)
			if err != nil {
				return "", err
			}
			path = "/" + orgSlug + localPath
		}
	}
	if path == "" {
		return link, nil
	}
	parsed.Path, parsed.RawPath = path, ""
	if s.config.Hosted != nil {
		public, err := url.Parse(s.config.Hosted.PublicURL)
		if err != nil {
			return "", err
		}
		parsed.Scheme, parsed.Host = public.Scheme, public.Host
	}
	return parsed.String(), nil
}

func (s *Service) resolveReadablePage(c echo.Context) error {
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	path := "/" + c.Param("project_slug")
	if number := c.Param("number"); number != "" {
		path += "/issues/" + number
	}
	project, id, _, err := s.readablePage(c.Request().Context(), credential, path)
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	return c.JSON(http.StatusOK, map[string]string{"project_id": project, "work_item_id": id})
}

func (s *Service) readablePage(ctx context.Context, credential hubapi.APICredential, path string) (string, string, string, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	valid := len(parts) == 1 || len(parts) == 3 && parts[1] == "issues" || len(parts) == 5 && parts[1] == "issues" && parts[3] == "changes"
	if !valid {
		return "", "", "", hubapi.NativeNotFound()
	}
	return s.readablePageRecord(ctx, credential, parts)
}

func (s *Service) readableShellPath(c echo.Context) (string, bool, error) {
	repository := s.config.TenantRepository
	if repository == nil {
		return "", false, hubapi.NativeWorkItemStorageUnavailable()
	}
	path := c.Request().URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	orgSlug, err := s.readableOrganizationSlug(c.Request().Context())
	if err != nil {
		return "", false, err
	}
	requestedOrgSlug := ""
	if requested, ok := c.Get("readable_organization_slug").(string); ok {
		requestedOrgSlug = requested
	}
	if requestedOrgSlug == "" && !s.hostedShared() && len(parts) >= 2 {
		owned := false
		if err := repository.ReadTenant(c.Request().Context(), tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			owned, err = r.AccessOrganizationSlug(c.Request().Context(), parts[0])
			return err
		}); err != nil {
			return "", false, err
		}
		if owned {
			requestedOrgSlug = parts[0]
			path = "/" + strings.Join(parts[1:], "/")
			parts = parts[1:]
		}
	}
	legacyIssue := len(parts) >= 3 && (parts[0] == "work" && parts[1] == "i" || parts[0] == "chat" && parts[1] == "issues")
	legacyProjectIssue := len(parts) >= 4 && parts[0] == "projects" && parts[2] == "issues"
	legacyProject := len(parts) == 3 && parts[0] == "work" && parts[1] == "p"
	if !legacyIssue && !legacyProjectIssue && !legacyProject && readableurl.Validate(parts[0]) != nil {
		if requestedOrgSlug != "" && requestedOrgSlug != orgSlug {
			return "/" + orgSlug + path, true, nil
		}
		if requestedOrgSlug == "" && orgSlug != "" && s.hostedShared() {
			if path == "/" {
				path = "/work"
			}
			return "/" + orgSlug + path, true, nil
		}
		return "", false, nil
	}
	credential, _, err := s.appCredential(c)
	if err != nil {
		if classified := hubscope.OperatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			return "", false, classified
		}
		return "", false, hubapi.NativeNotFound()
	}
	if legacyIssue || legacyProjectIssue || legacyProject {
		kind := "project"
		if legacyIssue {
			kind = "issue"
		} else if legacyProjectIssue {
			kind = "project_issue"
		}
		target, err := s.readableLegacyRecordPath(c.Request().Context(), credential, parts, kind)
		return target, true, err
	}
	if len(parts) > 0 && readableurl.Validate(parts[0]) == nil {
		_, _, target, err := s.readablePage(c.Request().Context(), credential, path)
		return target, true, err
	}
	return "", false, nil
}

func (s *Service) normalizeToolReferences(ctx context.Context, call operatortool.Call) (operatortool.Call, error) {
	if len(call.Arguments) > operatortool.ArgumentByteLimit(call.Name) {
		return call, operatortool.ErrInvalidArguments
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return call, nil
	}
	changed := false
	var project string
	if value := fields["project_id"]; value != nil {
		if err := json.Unmarshal(value, &project); err != nil {
			return call, operatortool.ErrInvalidArguments
		}
	}
	for _, key := range []string{"reference", "identifier", "work_item_id", "related", "subject_work_item_id"} {
		var reference string
		if value := fields[key]; value == nil || json.Unmarshal(value, &reference) != nil || !strings.Contains(reference, "/") {
			continue
		}
		authorized, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: project})
		if err != nil {
			return call, err
		}
		resolver, ok := authorized.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
		if !ok {
			return call, operatortool.ErrAccessDenied
		}
		scope, err := resolver(authorized)
		if err != nil {
			return call, err
		}
		scope.Project = tracker.ProjectID(project)
		id, err := s.readableReference(authorized, scope, reference)
		if err != nil {
			return call, hubapi.SafeWorkReadError(err)
		}
		fields[key], err = json.Marshal(id)
		if err != nil {
			return call, err
		}
		changed = true
	}
	if input := fields["input"]; input != nil {
		var nested map[string]json.RawMessage
		if json.Unmarshal(input, &nested) == nil && nested != nil {
			originalProject, hadProject := nested["project_id"]
			nested["project_id"] = fields["project_id"]
			raw, err := json.Marshal(nested)
			if err != nil {
				return call, err
			}
			normalized, err := s.normalizeToolReferences(ctx, operatortool.Call{Name: call.Name, Arguments: raw})
			if err != nil {
				return call, err
			}
			if err := json.Unmarshal(normalized.Arguments, &nested); err != nil {
				return call, err
			}
			if hadProject {
				nested["project_id"] = originalProject
			} else {
				delete(nested, "project_id")
			}
			fields["input"], err = json.Marshal(nested)
			if err != nil {
				return call, err
			}
			changed = true
		}
	}
	if changed {
		raw, err := json.Marshal(fields)
		if err != nil {
			return call, err
		}
		call.Arguments = raw
	}
	return call, nil
}
