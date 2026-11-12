package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const workspaceCountWarningThreshold = 50

type GitWorktree struct {
	Path   string
	Branch string
}

func CheckWorkspaceGrowth(
	ctx context.Context,
	projectID string,
	workspaceRoot string,
	sourceRoot string,
	deps Deps,
) (Check, bool) {
	name := "Project " + projectID + " workspace growth"
	resolvedRoot, err := ExpandWorkspacePath(workspaceRoot)
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		if err == nil {
			err = errors.New("workspace.root is not configured")
		}
		return Check{
			Name:   name,
			Status: Warn,
			Detail: fmt.Sprintf("cannot inspect workspace growth: %v", err),
			Hint:   "Set workspace.root to the directory Detent owns, then rerun detent doctor.",
		}, true
	}
	entries, err := os.ReadDir(resolvedRoot)
	if errors.Is(err, os.ErrNotExist) {
		return Check{}, false
	}
	if err != nil {
		return Check{
			Name:   name,
			Status: Warn,
			Detail: fmt.Sprintf("cannot inspect workspace.root %s: %v", resolvedRoot, err),
			Hint:   "Verify workspace.root is readable, then rerun detent doctor.",
		}, true
	}
	if strings.TrimSpace(sourceRoot) != "" {
		resolvedSourceRoot, sourceErr := ExpandWorkspacePath(sourceRoot)
		if sourceErr == nil && canonicalPath(resolvedRoot) == canonicalPath(resolvedSourceRoot) {
			return Check{}, false
		}
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() != ".detent" && entry.IsDir() {
			count++
		}
	}
	if count < workspaceCountWarningThreshold {
		return Check{}, false
	}
	detail := fmt.Sprintf("workspace.root %s contains %d retained workspace directories and reached the %d-directory warning threshold", resolvedRoot, count, workspaceCountWarningThreshold)
	if deps.GitWorktrees != nil && strings.TrimSpace(sourceRoot) != "" {
		unregistered, classificationErr := countDoctorUnregisteredWorkspaceDirectories(ctx, resolvedRoot, sourceRoot, entries, deps)
		if classificationErr != nil {
			detail += fmt.Sprintf("; cannot classify unregistered directories: %v", classificationErr)
		} else {
			detail += fmt.Sprintf("; %d are not registered with the source repository", unregistered)
		}
	}
	return Check{
		Name:   name,
		Status: Warn,
		Detail: detail,
		Hint:   "Confirm workspace cleanup is running and inspect active work before removing stale workspaces.",
	}, true
}

func countDoctorUnregisteredWorkspaceDirectories(
	ctx context.Context,
	resolvedRoot string,
	sourceRoot string,
	entries []os.DirEntry,
	deps Deps,
) (int, error) {
	resolvedSourceRoot, err := ExpandWorkspacePath(sourceRoot)
	if err != nil {
		return 0, err
	}
	worktrees, err := deps.GitWorktrees(ctx, resolvedSourceRoot)
	if err != nil {
		return 0, err
	}
	registered := make(map[string]struct{}, len(worktrees))
	for _, worktree := range worktrees {
		registered[canonicalPath(worktree.Path)] = struct{}{}
	}
	unregistered := 0
	for _, entry := range entries {
		if entry.Name() == ".detent" || !entry.IsDir() {
			continue
		}
		path := canonicalPath(filepath.Join(resolvedRoot, entry.Name()))
		if _, ok := registered[path]; !ok {
			unregistered++
		}
	}
	return unregistered, nil
}

func CheckExternalBranchWorktrees(
	ctx context.Context,
	projectID string,
	sourceRoot string,
	workspaceRoot string,
	deps Deps,
) Check {
	name := "Project " + projectID + " external branch worktrees"
	if deps.GitWorktrees == nil {
		return Check{Name: name, Status: OK, Detail: "skipped because the worktree inspector is unavailable"}
	}
	resolvedRoot, err := ExpandWorkspacePath(workspaceRoot)
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		if err == nil {
			err = errors.New("workspace.root is not configured")
		}
		return Check{
			Name:   name,
			Status: Warn,
			Detail: fmt.Sprintf("cannot inspect external branch worktrees: %v", err),
			Hint:   "Set workspace.root to the directory Detent owns, then rerun detent doctor.",
		}
	}
	worktrees, err := deps.GitWorktrees(ctx, sourceRoot)
	if err != nil {
		return Check{
			Name:   name,
			Status: Warn,
			Detail: fmt.Sprintf("cannot list worktrees registered with %s: %v", sourceRoot, err),
			Hint:   "Verify the source repository, then rerun detent doctor.",
		}
	}
	sourceRoot = canonicalPath(sourceRoot)
	resolvedRoot = canonicalPath(resolvedRoot)
	external := make([]GitWorktree, 0)
	for _, worktree := range worktrees {
		worktree.Path = canonicalPath(worktree.Path)
		worktree.Branch = strings.TrimSpace(worktree.Branch)
		if worktree.Branch == "" || worktree.Path == "" || worktree.Path == sourceRoot || pathWithin(resolvedRoot, worktree.Path) {
			continue
		}
		external = append(external, worktree)
	}
	sort.Slice(external, func(left, right int) bool {
		if external[left].Branch == external[right].Branch {
			return external[left].Path < external[right].Path
		}
		return external[left].Branch < external[right].Branch
	})
	if len(external) == 0 {
		return Check{
			Name:   name,
			Status: OK,
			Detail: "all branch worktrees are the configured source checkout or within workspace.root " + resolvedRoot,
		}
	}
	details := make([]string, 0, len(external))
	for _, worktree := range external {
		details = append(details, worktree.Branch+" at "+worktree.Path)
	}
	summary := fmt.Sprintf("%d branches are held", len(external))
	if len(external) == 1 {
		summary = "1 branch is held"
	}
	return Check{
		Name:   name,
		Status: Warn,
		Detail: fmt.Sprintf("%s by worktrees outside workspace.root %s: %s", summary, resolvedRoot, strings.Join(details, "; ")),
		Hint:   "Active PR review checkouts are safe to keep and Detent will resume when their branches are released. For genuinely dead entries, verify each path before running git worktree remove <path>, then run git worktree prune.",
	}
}

func DefaultGitWorktrees(ctx context.Context, sourceRoot string) ([]GitWorktree, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", "-C", sourceRoot, "worktree", "list", "--porcelain", "-z")
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return parseDoctorGitWorktrees(string(output)), nil
}

func parseDoctorGitWorktrees(output string) []GitWorktree {
	worktrees := make([]GitWorktree, 0)
	current := GitWorktree{}
	flush := func() {
		if strings.TrimSpace(current.Path) != "" {
			worktrees = append(worktrees, current)
		}
		current = GitWorktree{}
	}
	for _, field := range strings.Split(output, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			flush()
			current.Path = strings.TrimSpace(strings.TrimPrefix(field, "worktree "))
		case strings.HasPrefix(field, "branch "):
			branch := strings.TrimSpace(strings.TrimPrefix(field, "branch "))
			current.Branch = strings.TrimPrefix(branch, "refs/heads/")
		}
	}
	flush()
	return worktrees
}

func canonicalPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func pathWithin(root string, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
