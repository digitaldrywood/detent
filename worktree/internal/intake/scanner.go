package intake

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxScannedFileBytes = 1 << 20

var todoPattern = regexp.MustCompile(`(?:^|[^\w-])(TODO|FIXME)\b[\s:=-]*(.*)$`)

// todoMarker requires an explicit marker delimiter or a marker immediately
// following a comment leader, rather than a mention in prose.
func todoMarker(line, path string) []string {
	indices := todoPattern.FindStringSubmatchIndex(line)
	if indices == nil {
		return nil
	}
	prefix := strings.TrimSpace(line[:indices[2]])
	suffix := strings.TrimSpace(line[indices[3]:])
	explicit := strings.HasPrefix(suffix, ":") || strings.HasPrefix(suffix, "(")
	comment := false
	for _, leader := range []string{"//", "/*", "*", "#", "<!--", "--"} {
		if leader == "#" && strings.EqualFold(filepath.Ext(path), ".md") {
			continue
		}
		if strings.HasSuffix(prefix, leader) {
			comment = true
			break
		}
	}
	if !explicit && !comment {
		return nil
	}
	return todoPattern.FindStringSubmatch(line)
}

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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	files, err := s.revisionFiles(ctx)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return []Event{}, nil
	}
	var input strings.Builder
	for _, file := range files {
		input.WriteString(file.object + "\n")
	}
	cmd := s.gitCommand(ctx, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(input.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	events, scanErr := scanTODOFiles(ctx, files, bufio.NewReader(output))
	contextErr := ctx.Err()
	if scanErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if contextErr != nil {
		return nil, contextErr
	}
	if scanErr != nil {
		return nil, fmt.Errorf("%w: %s", scanErr, strings.TrimSpace(stderr.String()))
	}
	if waitErr != nil {
		return nil, gitScanError(ctx, "cat-file", waitErr, stderr.String())
	}
	return events, nil
}

func scanTODOFiles(ctx context.Context, files []revisionFile, output *bufio.Reader) ([]Event, error) {
	events := []Event{}
	for _, file := range files {
		path := file.path
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		header, err := output.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read stale TODO file %q: %w", path, err)
		}
		if header != fmt.Sprintf("%s blob %d\n", file.object, file.size) {
			return nil, fmt.Errorf("unexpected stale TODO blob header for %q: %q", path, header)
		}
		content := make([]byte, file.size+1)
		if _, err := io.ReadFull(output, content); err != nil {
			return nil, fmt.Errorf("read stale TODO file %q: %w", path, err)
		}
		if content[file.size] != '\n' {
			return nil, fmt.Errorf("invalid stale TODO blob delimiter for %q", path)
		}
		content = content[:file.size]
		scanner := bufio.NewScanner(bytes.NewReader(content))
		scanner.Buffer(make([]byte, 64*1024), maxScannedFileBytes)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			matches := todoMarker(line, path)
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
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("scan tracked stale TODO file %q: %w", path, err)
		}
	}
	return events, nil
}

type revisionFile struct {
	path   string
	object string
	size   int64
}

// revisionFiles pins the remote default branch before reading any tree or blob.
// Fetching by object ID without ref updates leaves the live checkout untouched.
func (s staleTODOScanner) revisionFiles(ctx context.Context) ([]revisionFile, error) {
	output, err := s.git(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return nil, fmt.Errorf("validate stale TODO source root: source root must be a Git worktree with git available: %w", err)
	}
	if strings.TrimSpace(string(output)) != "true" {
		return nil, errors.New("validate stale TODO source root: source root must be a Git worktree with git available")
	}
	output, err = s.git(ctx, "ls-remote", "--exit-code", "origin", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve stale TODO remote default branch: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[1] != "HEAD" {
		return nil, errors.New("resolve stale TODO remote default branch: origin must advertise HEAD")
	}
	revision := fields[0]
	if _, err := s.git(ctx, "fetch", "--no-tags", "--no-write-fetch-head", "--", "origin", revision); err != nil {
		return nil, fmt.Errorf("fetch stale TODO default-branch revision: %w", err)
	}
	output, err = s.git(ctx, "ls-tree", "-r", "-l", "-z", revision, "--", ".")
	if err != nil {
		return nil, fmt.Errorf("list stale TODO revision files: %w", err)
	}
	var files []revisionFile
	for entry := range strings.SplitSeq(string(output), "\x00") {
		if entry == "" {
			continue
		}
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 4 {
			return nil, errors.New("invalid stale TODO Git tree entry")
		}
		if fields[0] != "100644" && fields[0] != "100755" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse stale TODO blob size: %w", err)
		}
		if size > maxScannedFileBytes || !scannablePath(path) {
			continue
		}
		files = append(files, revisionFile{path: path, object: fields[2], size: size})
	}
	return files, nil
}

func (s staleTODOScanner) gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = append([]string{"git", "-C", s.root}, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	return cmd
}

func gitScanError(ctx context.Context, operation string, err error, stderr string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("git %s: %w: %s", operation, err, strings.TrimSpace(stderr))
}

func (s staleTODOScanner) git(ctx context.Context, operation string, args ...string) ([]byte, error) {
	cmd := s.gitCommand(ctx, append([]string{operation}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, gitScanError(ctx, operation, err, stderr.String())
	}
	return output, nil
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
