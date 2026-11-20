package gitexec

import (
	"context"
	"errors"
	"fmt"
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
	return parseRemoteBranchHead(output, ref)
}

func parseRemoteBranchHead(output, ref string) (string, bool, error) {
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
		if path == "" || strings.ContainsAny(remote, ":@") {
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

func RemoteDefaultBranch(ctx context.Context, workspacePath string, remote string) (string, error) {
	output, err := Run(ctx, workspacePath, RemoteArgs("ls-remote", "--symref", remote, "HEAD")...)
	return parseRemoteDefaultBranch(output, remote, err)
}

func parseRemoteDefaultBranch(output, remote string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "ref:" || fields[2] != "HEAD" {
			continue
		}
		branch := strings.TrimPrefix(fields[1], "refs/heads/")
		if branch != fields[1] && strings.TrimSpace(branch) != "" {
			return branch, nil
		}
	}
	return "", fmt.Errorf("remote %s HEAD is not a branch", remote)
}

func RemoteDefaultBranchHead(ctx context.Context, workspacePath string, remote string) (string, string, error) {
	branch, err := RemoteDefaultBranch(ctx, workspacePath, remote)
	if err != nil {
		return "", "", err
	}
	head, exists, err := RemoteBranchHead(ctx, workspacePath, remote, branch)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return "", "", fmt.Errorf("remote %s default branch %s is missing", remote, branch)
	}
	return branch, head, nil
}

func PublicationDefaultBranch(ctx context.Context, repository string) (string, error) {
	output, err := ReadPublicationRefs(ctx, repository, true, "HEAD")
	return parseRemoteDefaultBranch(output, repository, err)
}

func VerifyMergedBranch(ctx context.Context, path, remote, base, baseRef, mergeSHA string) error {
	if _, err := Run(ctx, path, "fetch", remote, "+refs/heads/"+base+":"+baseRef); err != nil {
		return fmt.Errorf("verify merged base branch: %w", err)
	}
	if _, err := Run(ctx, path, "merge-base", "--is-ancestor", mergeSHA, baseRef); err != nil {
		return fmt.Errorf("GitHub merge commit %s is not on %s: %w", mergeSHA, base, err)
	}
	return nil
}
