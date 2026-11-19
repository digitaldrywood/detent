package projectadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReadableLinks struct {
	PublicURL        *string
	HostedBase       func() string
	IssuePath        func(context.Context, string) (string, error)
	IssueAliasPath   func(context.Context, []string) (string, bool, error)
	ProjectPath      func(context.Context, tracker.ProjectID) (string, error)
	OrganizationSlug func(context.Context) (string, error)
}

func (links ReadableLinks) ToolResult(ctx context.Context, result operatortool.Result, name string) (operatortool.Result, error) {
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
					canonical, err := links.BrowserLink(ctx, link)
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

func (links ReadableLinks) BrowserLink(ctx context.Context, link string) (string, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return link, nil
	}
	if parsed.Host != "" {
		if links.PublicURL == nil {
			return link, nil
		}
		public, err := url.Parse(*links.PublicURL)
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
			path, err = links.IssuePath(ctx, part)
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
		var found bool
		path, found, err = links.IssueAliasPath(ctx, parts)
		if err != nil {
			return "", err
		}
		if found && len(parts) > 4 {
			path += "/" + strings.Join(parts[4:], "/")
		}
	}
	if path == "" {
		for i, part := range parts {
			if strings.HasPrefix(part, "prj_") && i > 0 && (parts[i-1] == "projects" || parts[i-1] == "p") && i == len(parts)-1 {
				path, err = links.ProjectPath(ctx, tracker.ProjectID(part))
				if err != nil {
					return "", err
				}
				break
			}
		}
	}
	if path == "" && links.PublicURL != nil {
		localPath := strings.TrimPrefix(parsed.Path, links.HostedBase())
		first := strings.Split(strings.Trim(localPath, "/"), "/")[0]
		if localPath == "" || localPath == "/" {
			localPath = "/work"
			first = "work"
		}
		if slices.Contains([]string{"work", "chat", "settings", "projects", "organization", "fleet", "reports", "diagnostics", "support"}, first) {
			orgSlug, err := links.OrganizationSlug(ctx)
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
	if links.PublicURL != nil {
		public, err := url.Parse(*links.PublicURL)
		if err != nil {
			return "", err
		}
		parsed.Scheme, parsed.Host = public.Scheme, public.Host
	}
	return parsed.String(), nil
}
