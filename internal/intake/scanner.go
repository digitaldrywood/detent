package intake

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	root := os.DirFS(s.root)
	events := []Event{}
	err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && skippedDirectory(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !scannablePath(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxScannedFileBytes {
			return nil
		}
		file, err := root.Open(path)
		if err != nil {
			return err
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
		return errors.Join(scanner.Err(), file.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("walk stale TODO source: %w", err)
	}
	return events, nil
}

func skippedDirectory(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".git", ".detent", "node_modules", "vendor", "tmp", "dist", "build":
		return true
	default:
		return false
	}
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
