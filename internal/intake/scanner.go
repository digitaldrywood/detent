package intake

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxScannedFileBytes = 1 << 20

var todoPattern = regexp.MustCompile(`(?i)\b(TODO|FIXME)\b[\s:=-]*(.*)$`)

type scannerFactory struct{}

type staleTODOScanner struct {
	root string
}

func DefaultScannerFactory() ScannerFactory {
	return scannerFactory{}
}

func (scannerFactory) New(name string, root string) (Scanner, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "stale-todos", "stale_todos":
		root = strings.TrimSpace(root)
		if root == "" {
			return nil, fmt.Errorf("%w: source root is required", ErrUnknownScanner)
		}
		return staleTODOScanner{root: root}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownScanner, name)
	}
}

func (s staleTODOScanner) Scan(ctx context.Context) ([]Event, error) {
	paths, err := s.trackedPaths(ctx)
	if err != nil {
		return nil, err
	}

	root := os.DirFS(s.root)
	events := []Event{}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !scannablePath(path) {
			continue
		}

		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(path)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect tracked stale TODO file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxScannedFileBytes {
			continue
		}

		file, err := root.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open tracked stale TODO file %q: %w", path, err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), maxScannedFileBytes)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			matches := todoPattern.FindStringSubmatch(line)
			if len(matches) != 3 {
				continue
			}
			detail := strings.TrimSpace(matches[2])
			summary := matches[1] + " in " + filepath.ToSlash(path) + fmt.Sprintf(":%d", lineNumber)
			if detail != "" {
				summary += ": " + detail
			}
			events = append(events, Event{
				Summary:     summary,
				Details:     "Location: " + filepath.ToSlash(path) + fmt.Sprintf(":%d\n\n", lineNumber) + line,
				Fingerprint: filepath.ToSlash(path) + "\x00" + strings.ToLower(strings.Join(strings.Fields(line), " ")),
				Fields: map[string]string{
					"path": filepath.ToSlash(path),
					"line": strconv.Itoa(lineNumber),
					"todo": detail,
				},
			})
		}
		if err := errors.Join(scanner.Err(), file.Close()); err != nil {
			return nil, fmt.Errorf("scan tracked stale TODO file %q: %w", path, err)
		}
	}
	return events, nil
}

func (s staleTODOScanner) trackedPaths(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = []string{"git", "-C", s.root, "rev-parse", "--is-inside-work-tree"}
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("validate stale TODO source root: %w; source root must be a Git worktree with git available", err)
	}
	if strings.TrimSpace(string(output)) != "true" {
		return nil, errors.New("validate stale TODO source root: source root must be a Git worktree")
	}

	cmd = exec.CommandContext(ctx, "git")
	cmd.Args = []string{"git", "-C", s.root, "ls-files", "--cached", "-z", "--", "."}
	output, err = cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("list git-tracked files for stale TODO scan: %w; source root must be a Git worktree with git available", err)
	}

	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		return nil, nil
	}
	return paths, nil
}

func scannablePath(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".py", ".rb", ".rs", ".java", ".kt", ".c", ".h", ".cc", ".cpp", ".cs", ".sh", ".bash", ".zsh", ".yaml", ".yml", ".json", ".toml", ".md", ".html", ".css", ".scss", ".sql", ".templ":
		return true
	default:
		return false
	}
}
