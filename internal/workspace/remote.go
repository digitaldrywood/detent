package workspace

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
)

// RepositoryURL is the https form of the checkout's configured origin URL,
// the reference a published Change Request version names as its repository.
// It reads the configured value rather than the effective one, so a
// url.<base>.insteadOf rewrite that sends transport elsewhere does not turn
// the repository's name into a local path. It is empty when the remote is
// not one an https URL can name.
func RepositoryURL(ctx context.Context, workspacePath string) string {
	output, err := runGitAt(ctx, workspacePath, "config", "--get", "remote."+defaultGitRemote+".url")
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

func publicationRepository(path, remote string) string {
	remote = strings.TrimSuffix(remote, "\n")
	if remote == "" || strings.TrimSpace(remote) != remote || strings.ContainsAny(remote, "\r\n") {
		return ""
	}
	if repository := HTTPSRemoteURL(remote); repository != "" {
		if parsed, err := url.Parse(remote); err == nil && parsed.Scheme != "https" && parsed.Port() != "" && parsed.Port() != "22" {
			return ""
		}
		return repository
	}
	if strings.HasPrefix(remote, "file://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return ""
		}
		remote = filepath.FromSlash(parsed.Path)
	}
	if !filepath.IsAbs(remote) {
		if strings.ContainsAny(remote, ":@") {
			return ""
		}
		remote = path + string(filepath.Separator) + remote
	}
	absolute, err := filepath.EvalSymlinks(remote)
	if err != nil {
		return ""
	}
	return absolute
}

func verifyPublicationRemote(ctx context.Context, path, remote, repository string) error {
	fetchURL, err := runGitAt(ctx, path, "remote", "get-url", "--all", remote)
	if err != nil || repository == "" || publicationRepository(path, fetchURL) != repository {
		return refuse(LandRefusalProtected, "the publication fetch remote differs from the configured project repository; restore its configured destination before retrying")
	}
	pushURL, err := runGitAt(ctx, path, "remote", "get-url", "--push", "--all", remote)
	if err != nil || publicationRepository(path, pushURL) != repository {
		return refuse(LandRefusalProtected, "the publication push remote differs from the configured project repository; restore its configured push destination before retrying")
	}
	return nil
}
