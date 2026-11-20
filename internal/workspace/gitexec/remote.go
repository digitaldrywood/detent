package gitexec

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

func RemoteArgs(args ...string) []string {
	if _, err := exec.LookPath("gh"); err != nil {
		return args
	}
	return append([]string{"-c", "credential.https://github.com.helper=!gh auth git-credential"}, args...)
}

func RemoteBranchHead(ctx context.Context, workspacePath string, remote string, branch string) (string, bool, error) {
	ref := "refs/heads/" + branch
	output, err := Run(ctx, workspacePath, RemoteArgs("ls-remote", remote, ref)...)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != ref {
			continue
		}
		return fields[0], true, nil
	}
	return "", false, nil
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

func PublicationRepository(path, remote string) string {
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

func VerifyPublicationRemote(ctx context.Context, path, remote, repository string) error {
	fetchURL, err := Run(ctx, path, "remote", "get-url", "--all", remote)
	if err != nil || repository == "" || PublicationRepository(path, fetchURL) != repository {
		return errors.New("the publication fetch remote differs from the configured project repository; restore its configured destination before retrying")
	}
	pushURL, err := Run(ctx, path, "remote", "get-url", "--push", "--all", remote)
	if err != nil || PublicationRepository(path, pushURL) != repository {
		return errors.New("the publication push remote differs from the configured project repository; restore its configured push destination before retrying")
	}
	return nil
}
