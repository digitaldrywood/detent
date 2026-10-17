package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func issueBrowserPath(organization, project string, number int) string {
	return "/" + organization + "/" + project + "/issues/" + strconv.Itoa(number)
}

func readIssueBrowserPath(ctx context.Context, query nativeQueryer, id string) (string, error) {
	var organization, project string
	var number int
	if err := query.QueryRowContext(ctx, `SELECT o.slug,p.slug,i.number FROM issues i JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id WHERE i.native_id=?`, id).Scan(&organization, &project, &number); err != nil {
		return "", err
	}
	return issueBrowserPath(organization, project, number), nil
}

func (s *Service) issueBrowserURL(ctx context.Context, id string) (string, error) {
	path, err := readIssueBrowserPath(ctx, s.database.reader, id)
	if err != nil {
		return "", err
	}
	if s.config.Hosted != nil {
		path = strings.TrimRight(s.config.Hosted.PublicURL, "/") + path
	}
	return path, nil
}

func setOrganizationSlug(ctx context.Context, tx *sql.Tx, id, slug string) error {
	if err := readableurl.Validate(slug); err != nil {
		return nativeInvalid(err.Error())
	}
	var owner string
	err := tx.QueryRowContext(ctx, "SELECT organization_id FROM organization_slugs WHERE slug=?", slug).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && owner != id {
		return nativeInvalid("This slug is already reserved")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO organization_slugs(slug,organization_id) VALUES(?,?) ON CONFLICT DO NOTHING", slug, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE organizations SET slug=? WHERE id=?", slug, id)
	return err
}

func (s *Service) syncOrganizationSlug(ctx context.Context, slug string) error {
	if slug == "" {
		return nil
	}
	var current string
	if err := s.database.reader.QueryRowContext(ctx, "SELECT slug FROM organizations WHERE id=?", s.config.Hosted.OrganizationID).Scan(&current); err != nil {
		return err
	}
	if current == slug {
		return nil
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setOrganizationSlug(ctx, tx, s.config.Hosted.OrganizationID, slug); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) slugSettings(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	project := c.Param("project")
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project), credential: credential}
	if project != "" {
		if err := s.requireHostedProject(ctx, s.database.reader, scope, false); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	var current string
	query, args := "SELECT slug FROM organizations WHERE id=?", []any{scope.organization}
	if project != "" {
		query, args = "SELECT slug FROM projects WHERE organization_id=? AND id=?", []any{scope.organization, project}
	}
	if c.Request().Method == http.MethodGet {
		if err := s.database.reader.QueryRowContext(ctx, query, args...).Scan(&current); err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, map[string]string{"slug": current})
	}
	if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if project == "" && s.hostedShared() {
		return s.nativeAPIError(c, nativeInvalid("Edit the organization slug through the shared entry"))
	}
	var input struct {
		Slug         string `json:"slug"`
		ExpectedSlug string `json:"expected_slug"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := readableurl.Validate(input.Slug); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	scope.requireHostedAdmin = true
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&current); err != nil {
		return s.nativeAPIError(c, err)
	}
	if current != input.ExpectedSlug {
		return s.nativeAPIError(c, nativeInvalid("The slug changed; reload and try again"))
	}
	if project == "" {
		err = setOrganizationSlug(ctx, tx, string(scope.organization), input.Slug)
	} else {
		var owner string
		err = tx.QueryRowContext(ctx, "SELECT project_id FROM project_slugs WHERE organization_id=? AND slug=?", scope.organization, input.Slug).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		if err == nil && owner != "" && owner != project {
			err = nativeInvalid("This slug is already reserved")
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO project_slugs(organization_id,slug,project_id) VALUES(?,?,?) ON CONFLICT DO NOTHING", scope.organization, input.Slug, project)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE projects SET slug=? WHERE organization_id=? AND id=?", input.Slug, scope.organization, project)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"slug": input.Slug})
}

func (s *Service) readableReference(ctx context.Context, query nativeQueryer, scope nativeScope, reference string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nativeNotFound()
	}
	if parsed.IsAbs() {
		if s.config.Hosted == nil {
			return "", nativeNotFound()
		}
		public, err := url.Parse(s.config.Hosted.PublicURL)
		if err != nil || parsed.Scheme != public.Scheme || parsed.Host != public.Host {
			return "", nativeNotFound()
		}
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return "", nativeNotFound()
	}
	var id string
	err = query.QueryRowContext(ctx, `SELECT i.native_id FROM organization_slugs o JOIN project_slugs p ON p.organization_id=o.organization_id JOIN issues i ON i.project_id=p.project_id WHERE o.slug=? AND p.slug=? AND i.number=? AND i.organization_id=? AND i.project_id=?`, parts[0], parts[1], parts[3], scope.organization, scope.project).Scan(&id)
	return id, err
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
			path, err = readIssueBrowserPath(ctx, s.database.reader, part)
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
		var id string
		err := s.database.reader.QueryRowContext(ctx, `SELECT i.native_id FROM organization_slugs o JOIN project_slugs p ON p.organization_id=o.organization_id JOIN issues i ON i.project_id=p.project_id WHERE o.slug=? AND p.slug=? AND i.number=?`, parts[0], parts[1], parts[3]).Scan(&id)
		if err == nil {
			path, err = readIssueBrowserPath(ctx, s.database.reader, id)
			if err != nil {
				return "", err
			}
			if len(parts) > 4 {
				path += "/" + strings.Join(parts[4:], "/")
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	if path == "" {
		for i, part := range parts {
			if strings.HasPrefix(part, "prj_") && i > 0 && (parts[i-1] == "projects" || parts[i-1] == "p") && i == len(parts)-1 {
				var orgSlug, projectSlug string
				if err := s.database.reader.QueryRowContext(ctx, `SELECT o.slug,p.slug FROM projects p JOIN organizations o ON o.id=p.organization_id WHERE p.id=?`, part).Scan(&orgSlug, &projectSlug); err != nil {
					return "", err
				}
				path = "/" + orgSlug + "/" + projectSlug
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
			var orgSlug string
			if err := s.database.reader.QueryRowContext(ctx, "SELECT slug FROM organizations WHERE id=?", s.config.Hosted.OrganizationID).Scan(&orgSlug); err != nil {
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
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	path := "/" + c.Param("project_slug")
	if number := c.Param("number"); number != "" {
		path += "/issues/" + number
	}
	project, id, _, err := s.readablePage(c.Request().Context(), credential, path)
	if err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return c.JSON(http.StatusOK, map[string]string{"project_id": project, "work_item_id": id})
}

func (s *Service) readablePage(ctx context.Context, credential apiCredential, path string) (string, string, string, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	valid := len(parts) == 1 || len(parts) == 3 && parts[1] == "issues" || len(parts) == 5 && parts[1] == "issues" && parts[3] == "changes"
	if !valid {
		return "", "", "", nativeNotFound()
	}
	var project, projectSlug, orgSlug string
	err := s.database.reader.QueryRowContext(ctx, `SELECT p.id,p.slug,o.slug FROM project_slugs a JOIN projects p ON p.id=a.project_id JOIN organizations o ON o.id=p.organization_id WHERE a.organization_id=? AND a.slug=?`, s.config.Hosted.OrganizationID, parts[0]).Scan(&project, &projectSlug, &orgSlug)
	if err != nil {
		return "", "", "", err
	}
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project), credential: credential}
	if err := s.requireHostedProject(ctx, s.database.reader, scope, false); err != nil {
		return "", "", "", err
	}
	canonical := "/" + orgSlug + "/" + projectSlug
	if len(parts) == 1 {
		return project, "", canonical, nil
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil || number < 1 {
		return "", "", "", nativeNotFound()
	}
	var id string
	if err := s.database.reader.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND number=?", scope.organization, scope.project, number).Scan(&id); err != nil {
		return "", "", "", err
	}
	if err := requireReporterItem(ctx, s.database.reader, scope, id); err != nil {
		return "", "", "", err
	}
	canonical = issueBrowserPath(orgSlug, projectSlug, number)
	if len(parts) == 5 {
		var count int
		if err := s.database.reader.QueryRowContext(ctx, `SELECT count(*) FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? AND c.id=?`, scope.organization, scope.project, id, parts[4]).Scan(&count); err != nil {
			return "", "", "", err
		}
		if count != 1 {
			return "", "", "", nativeNotFound()
		}
		canonical += "/changes/" + url.PathEscape(parts[4])
	}
	return project, id, canonical, nil
}

func (s *Service) readableShellPath(c echo.Context) (string, bool, error) {
	path := c.Request().URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var orgSlug string
	if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT slug FROM organizations WHERE id=?", s.config.Hosted.OrganizationID).Scan(&orgSlug); err != nil {
		return "", false, err
	}
	requestedOrgSlug := ""
	if requested, ok := c.Get("readable_organization_slug").(string); ok {
		requestedOrgSlug = requested
	}
	if requestedOrgSlug == "" && !s.hostedShared() && len(parts) >= 2 {
		var owner string
		if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT organization_id FROM organization_slugs WHERE slug=?", parts[0]).Scan(&owner); err == nil && owner == s.config.Hosted.OrganizationID {
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
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		return "", false, nativeNotFound()
	}
	if legacyIssue {
		id := parts[2]
		var project string
		if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT project_id FROM issues WHERE organization_id=? AND native_id=?", s.config.Hosted.OrganizationID, id).Scan(&project); err != nil {
			return "", false, err
		}
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project), credential: credential}
		if err := s.requireHostedProject(c.Request().Context(), s.database.reader, scope, false); err != nil {
			return "", false, err
		}
		if err := requireReporterItem(c.Request().Context(), s.database.reader, scope, id); err != nil {
			return "", false, err
		}
		target, err := s.issueBrowserURL(c.Request().Context(), id)
		if len(parts) > 3 {
			target += "/" + strings.Join(parts[3:], "/")
		}
		return target, true, err
	}
	if legacyProjectIssue {
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(parts[1]), credential: credential}
		if err := s.requireHostedProject(c.Request().Context(), s.database.reader, scope, false); err != nil {
			return "", false, err
		}
		issue, err := s.resolveOperatorNativeItem(c.Request().Context(), s.database.reader, scope, parts[3])
		if err != nil {
			return "", false, err
		}
		target := issue.WebURL
		if len(parts) > 4 {
			target += "/" + strings.Join(parts[4:], "/")
		}
		return target, true, err
	}
	if legacyProject {
		var slug string
		if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT slug FROM projects WHERE organization_id=? AND id=?", s.config.Hosted.OrganizationID, parts[2]).Scan(&slug); err != nil {
			return "", false, err
		}
		path = "/" + slug
		parts = []string{slug}
	}
	if len(parts) > 0 && readableurl.Validate(parts[0]) == nil {
		_, _, target, err := s.readablePage(c.Request().Context(), credential, path)
		return target, true, err
	}
	return "", false, nil
}

func (s *Service) normalizeToolReferences(ctx context.Context, call operatortool.Call) (operatortool.Call, error) {
	if len(call.Arguments) > operatortool.MaxArgumentBytes {
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
		resolver, ok := authorized.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
		if !ok {
			return call, operatortool.ErrAccessDenied
		}
		scope, err := resolver(authorized)
		if err != nil {
			return call, err
		}
		scope.project = tracker.ProjectID(project)
		id, err := s.readableReference(authorized, s.database.reader, scope, reference)
		if err != nil {
			return call, safeWorkReadError(err)
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
