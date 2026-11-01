package cli

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

const doctorWorkspaceCountWarningThreshold = 50

type doctorGitWorktree struct {
	Path   string
	Branch string
}

func checkDoctorWorkspaceGrowth(
	ctx context.Context,
	projectID string,
	workspaceRoot string,
	sourceRoot string,
	deps doctorDeps,
) (doctorCheck, bool) {
	name := "Project " + projectID + " workspace growth"
	resolvedRoot, err := expandDoctorWorkspacePath(workspaceRoot)
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		if err == nil {
			err = errors.New("workspace.root is not configured")
		}
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("cannot inspect workspace growth: %v", err),
			Hint:   "Set workspace.root to the directory Detent owns, then rerun detent doctor.",
		}, true
	}
	entries, err := os.ReadDir(resolvedRoot)
	if errors.Is(err, os.ErrNotExist) {
		return doctorCheck{}, false
	}
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("cannot inspect workspace.root %s: %v", resolvedRoot, err),
			Hint:   "Verify workspace.root is readable, then rerun detent doctor.",
		}, true
	}
	if strings.TrimSpace(sourceRoot) != "" {
		resolvedSourceRoot, sourceErr := expandDoctorWorkspacePath(sourceRoot)
		if sourceErr == nil && doctorCanonicalPath(resolvedRoot) == doctorCanonicalPath(resolvedSourceRoot) {
			return doctorCheck{}, false
		}
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() != ".detent" && entry.IsDir() {
			count++
		}
	}
	if count < doctorWorkspaceCountWarningThreshold {
		return doctorCheck{}, false
	}
	detail := fmt.Sprintf("workspace.root %s contains %d retained workspace directories and reached the %d-directory warning threshold", resolvedRoot, count, doctorWorkspaceCountWarningThreshold)
	if deps.gitWorktrees != nil && strings.TrimSpace(sourceRoot) != "" {
		unregistered, classificationErr := countDoctorUnregisteredWorkspaceDirectories(ctx, resolvedRoot, sourceRoot, entries, deps)
		if classificationErr != nil {
			detail += fmt.Sprintf("; cannot classify unregistered directories: %v", classificationErr)
		} else {
			detail += fmt.Sprintf("; %d are not registered with the source repository", unregistered)
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: detail,
		Hint:   "Confirm workspace cleanup is running and inspect active work before removing stale workspaces.",
	}, true
}

func countDoctorUnregisteredWorkspaceDirectories(
	ctx context.Context,
	resolvedRoot string,
	sourceRoot string,
	entries []os.DirEntry,
	deps doctorDeps,
) (int, error) {
	resolvedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
	if err != nil {
		return 0, err
	}
	worktrees, err := deps.gitWorktrees(ctx, resolvedSourceRoot)
	if err != nil {
		return 0, err
	}
	registered := make(map[string]struct{}, len(worktrees))
	for _, worktree := range worktrees {
		registered[doctorCanonicalPath(worktree.Path)] = struct{}{}
	}
	unregistered := 0
	for _, entry := range entries {
		if entry.Name() == ".detent" || !entry.IsDir() {
			continue
		}
		path := doctorCanonicalPath(filepath.Join(resolvedRoot, entry.Name()))
		if _, ok := registered[path]; !ok {
			unregistered++
		}
	}
	return unregistered, nil
}

func checkDoctorExternalBranchWorktrees(
	ctx context.Context,
	projectID string,
	sourceRoot string,
	workspaceRoot string,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + projectID + " external branch worktrees"
	if deps.gitWorktrees == nil {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "skipped because the worktree inspector is unavailable"}
	}
	resolvedRoot, err := expandDoctorWorkspacePath(workspaceRoot)
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		if err == nil {
			err = errors.New("workspace.root is not configured")
		}
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("cannot inspect external branch worktrees: %v", err),
			Hint:   "Set workspace.root to the directory Detent owns, then rerun detent doctor.",
		}
	}
	worktrees, err := deps.gitWorktrees(ctx, sourceRoot)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("cannot list worktrees registered with %s: %v", sourceRoot, err),
			Hint:   "Verify the source repository, then rerun detent doctor.",
		}
	}
	sourceRoot = doctorCanonicalPath(sourceRoot)
	resolvedRoot = doctorCanonicalPath(resolvedRoot)
	external := make([]doctorGitWorktree, 0)
	for _, worktree := range worktrees {
		worktree.Path = doctorCanonicalPath(worktree.Path)
		worktree.Branch = strings.TrimSpace(worktree.Branch)
		if worktree.Branch == "" || worktree.Path == "" || worktree.Path == sourceRoot || doctorPathWithin(resolvedRoot, worktree.Path) {
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
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
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
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: fmt.Sprintf("%s by worktrees outside workspace.root %s: %s", summary, resolvedRoot, strings.Join(details, "; ")),
		Hint:   "Active PR review checkouts are safe to keep and Detent will resume when their branches are released. For genuinely dead entries, verify each path before running git worktree remove <path>, then run git worktree prune.",
	}
}

func defaultDoctorGitWorktrees(ctx context.Context, sourceRoot string) ([]doctorGitWorktree, error) {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", "-C", sourceRoot, "worktree", "list", "--porcelain", "-z") // #nosec G204 -- doctor runs a fixed read-only git command against the configured source root.
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

func parseDoctorGitWorktrees(output string) []doctorGitWorktree {
	worktrees := make([]doctorGitWorktree, 0)
	current := doctorGitWorktree{}
	flush := func() {
		if strings.TrimSpace(current.Path) != "" {
			worktrees = append(worktrees, current)
		}
		current = doctorGitWorktree{}
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

func doctorCanonicalPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func doctorPathWithin(root string, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
