package workpath

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const LegacyScratchRelativePath = ".detent/worker-tmp"

const WorkerScratchBaseName = "detent-worker-scratch"

// WorkerScratchRoot is the per-workspace parent of attempt scratch. It lives
// under the OS temp root rather than the workspace so that compiler and test
// churn never lands in a file-watched home tree. Workspaces are grouped by
// their parent directory so retention can sweep scratch of removed workspaces.
func WorkerScratchRoot(workspacePath string) string {
	workspacePath = BestEffortCanonicalPath(workspacePath)
	return filepath.Join(WorkerScratchGroup(filepath.Dir(workspacePath)), filepath.Base(workspacePath))
}

// WorkerScratchRoots lists the external scratch roots a workspace may own:
// the current per-user root and the root under the base an earlier runner
// shared between users.
func WorkerScratchRoots(workspacePath string) []string {
	workspacePath = BestEffortCanonicalPath(workspacePath)
	roots := []string{WorkerScratchRoot(workspacePath)}
	if shared := filepath.Join(WorkerScratchGroupIn(SharedWorkerScratchBase(), filepath.Dir(workspacePath)), filepath.Base(workspacePath)); shared != roots[0] {
		roots = append(roots, shared)
	}
	return roots
}

// WorkerScratchParent returns the directory that owns scratchPath: the
// external scratch root, the root under the base an earlier runner shared
// between users, or the legacy in-workspace scratch directory for attempts
// started before scratch moved out of the workspace.
func WorkerScratchParent(workspacePath string, scratchPath string) (string, bool) {
	scratchPath = filepath.Clean(scratchPath)
	for _, parent := range append(WorkerScratchRoots(workspacePath), filepath.Join(workspacePath, filepath.FromSlash(LegacyScratchRelativePath))) {
		if scratchPath == parent || PathWithin(parent, scratchPath) {
			return parent, true
		}
	}
	return "", false
}

func WorkerScratchGroup(workspaceParent string) string {
	return WorkerScratchGroupIn(WorkerScratchBase(), workspaceParent)
}

func WorkerScratchGroupIn(base string, workspaceParent string) string {
	sum := sha256.Sum256([]byte(BestEffortCanonicalPath(workspaceParent)))
	return filepath.Join(base, hex.EncodeToString(sum[:8]))
}

func WorkerScratchBase() string {
	base := SharedWorkerScratchBase()
	if uid := os.Getuid(); uid >= 0 {
		base += "-" + strconv.Itoa(uid)
	}
	return base
}

func SharedWorkerScratchBase() string {
	return filepath.Join(BestEffortCanonicalPath(os.TempDir()), WorkerScratchBaseName)
}

func BestEffortCanonicalPath(path string) string {
	if canonical, err := CanonicalExistingPath(path); err == nil {
		return canonical
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

func PathWithin(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

var ErrUnsafePath = errors.New("unsafe workspace path")

type PathError struct {
	Path   string
	Root   string
	Reason string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("%s: %s is not safe under %s: %s", ErrUnsafePath, e.Path, e.Root, e.Reason)
}

func (e *PathError) Unwrap() error {
	return ErrUnsafePath
}

func EnsurePrivateDirectories(base string, path string) error {
	if !PathWithin(base, path) {
		return &PathError{Path: path, Root: base, Reason: "directory escapes root"}
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return err
	}
	current := base
	components := []string{current}
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			components = append(components, current)
		}
	}
	for _, component := range components {
		if err := os.Mkdir(component, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := os.Lstat(component)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return &PathError{Path: component, Root: base, Reason: "scratch directory is not a directory"}
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
			return &PathError{Path: component, Root: base, Reason: "scratch directory is writable by others"}
		}
	}
	return nil
}

func PathExists(path string) (bool, bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		return true, info.IsDir(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	return false, false, fmt.Errorf("inspect path: %w", err)
}

func DirIsEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read workspace directory: %w", err)
	}
	return len(entries) == 0, nil
}

func CanonicalExistingPath(path string) (string, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(expanded) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", &os.PathError{Op: "canonicalize", Path: abs, Err: err}
	}
	return filepath.Clean(canonical), nil
}

func ExpandPath(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func AttemptKey(organization, project, attempt string) string {
	id, typed := strings.CutPrefix(attempt, "attempt_")
	if !typed || len(id) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	scope := attemptScope(organization, project)
	if scope == "" {
		return ""
	}
	return "attempt-" + scope + "-" + strings.ToLower(id)
}

func attemptScope(organization, project string) string {
	if organization == "" || project == "" {
		return ""
	}
	ids, _ := json.Marshal([2]string{organization, project})
	digest := sha256.Sum256(ids)
	return hex.EncodeToString(digest[:16])
}

func SplitCheckoutIssueKey(key string) (scope, issueKey string) {
	owner, issue, ok := strings.Cut(key, "--")
	if !ok || issue == "" {
		return "", key
	}
	owner, ok = strings.CutPrefix(owner, "attempt-")
	if !ok {
		return "", key
	}
	scope, attempt, ok := strings.Cut(owner, "-")
	if !ok || len(scope) != 32 || len(attempt) != 32 {
		return "", key
	}
	if _, err := hex.DecodeString(scope + attempt); err != nil {
		return "", key
	}
	return "attempt-" + owner, issue
}
