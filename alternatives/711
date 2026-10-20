package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type globalConfigGitStatus struct {
	RepositoryRoot string `json:"repository_root"`
	ResolvedPath   string `json:"resolved_path"`
	Tracked        bool   `json:"tracked"`
	Dirty          bool   `json:"dirty"`
	Status         string `json:"status"`
}

func inspectGlobalConfigGit(ctx context.Context, path string, run CommandRunner) *globalConfigGitStatus {
	if ctx == nil || run == nil || strings.TrimSpace(path) == "" {
		return nil
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil
	}
	resolved = filepath.Clean(resolved)

	rootOutput, err := run(ctx, "git", "-C", filepath.Dir(resolved), "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	repositoryRoot := strings.TrimSpace(rootOutput)
	if repositoryRoot == "" {
		return nil
	}
	repositoryRoot, err = filepath.Abs(repositoryRoot)
	if err != nil {
		return nil
	}
	repositoryRoot = filepath.Clean(repositoryRoot)
	relativePath, err := filepath.Rel(repositoryRoot, resolved)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return nil
	}
	relativePath = filepath.ToSlash(relativePath)

	_, trackedErr := run(ctx, "git", "-C", repositoryRoot, "ls-files", "--error-unmatch", "--", relativePath)
	statusOutput, err := run(ctx, "git", "-C", repositoryRoot, "status", "--short", "--untracked-files=all", "--", relativePath)
	if err != nil {
		return nil
	}
	status := strings.TrimRight(statusOutput, "\r\n")
	tracked := trackedErr == nil
	if status == "" {
		if tracked {
			status = "clean"
		} else {
			status = "untracked"
		}
	}

	return &globalConfigGitStatus{
		RepositoryRoot: repositoryRoot,
		ResolvedPath:   resolved,
		Tracked:        tracked,
		Dirty:          !tracked || status != "clean",
		Status:         status,
	}
}

func (s globalConfigGitStatus) trackedStatus() string {
	switch {
	case s.Tracked && !s.Dirty:
		return "tracked and clean"
	case s.Tracked:
		return fmt.Sprintf("tracked status %q", s.Status)
	case s.Status == "untracked":
		return "untracked"
	default:
		return fmt.Sprintf("untracked status %q", s.Status)
	}
}

func globalConfigGitDurabilityWarning(repositoryRoot string) string {
	return fmt.Sprintf("The registration is not durably recorded; a later git checkout or git reset in %s can discard it. Commit the global config change in that repository when ready.", repositoryRoot)
}
