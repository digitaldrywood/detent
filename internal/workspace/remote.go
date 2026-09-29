package workspace

import (
	"context"
	"net/url"
	"strings"
)

// RepositoryURL is the https form of the checkout's origin remote, the
// reference a published Change Request version names as its repository. It is
// empty when the remote is not one an https URL can name, such as a path on
// the local disk.
func RepositoryURL(ctx context.Context, workspacePath string) string {
	output, err := runGitAt(ctx, workspacePath, "remote", "get-url", defaultGitRemote)
	if err != nil {
		return ""
	}
	return HTTPSRemoteURL(output)
}

// HTTPSRemoteURL rewrites a git remote to the https URL of the same
// repository: https remotes keep their host and path, and ssh remotes in
// either the scp-like or the ssh:// form map to https on the same host. The
// trailing .git is dropped so the result can be extended with a host's
// commit path. Anything else, including local paths and other schemes,
// yields an empty string.
func HTTPSRemoteURL(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		switch parsed.Scheme {
		case "https":
			if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				return ""
			}
			return httpsRepository(parsed.Host, parsed.Path)
		case "ssh", "git+ssh", "ssh+git":
			return httpsRepository(parsed.Hostname(), parsed.Path)
		}
		return ""
	}
	if strings.Contains(remote, "://") {
		return ""
	}
	user, rest, ok := strings.Cut(remote, "@")
	if !ok || user == "" || strings.ContainsAny(user, "/\\") {
		return ""
	}
	host, path, ok := strings.Cut(rest, ":")
	if !ok || host == "" || strings.ContainsAny(host, "/\\") {
		return ""
	}
	return httpsRepository(host, path)
}

func httpsRepository(host, path string) string {
	path = strings.Trim(strings.TrimSuffix(strings.TrimSpace(path), ".git"), "/")
	if host == "" || path == "" || strings.ContainsAny(path, " \t\r\n?#") {
		return ""
	}
	return "https://" + host + "/" + path
}
