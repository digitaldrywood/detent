package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	diffStatFilesPattern   = regexp.MustCompile(`(\d+)\s+files?\s+changed`)
	diffStatAddedPattern   = regexp.MustCompile(`(\d+)\s+insertions?\(\+\)`)
	diffStatRemovedPattern = regexp.MustCompile(`(\d+)\s+deletions?\(-\)`)
)

type DiffStat struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

func (l *LocalGit) DiffStat(ctx context.Context, info Info, issue Issue) (DiffStat, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return DiffStat{}, err
	}
	return GitDiffStat(ctx, normalized.Path)
}

func GitDiffStat(ctx context.Context, workspacePath string) (DiffStat, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return DiffStat{}, errors.New("workspace path is required")
	}

	output, err := gitDiffStatOutput(ctx, workspacePath)
	if err != nil {
		return DiffStat{}, err
	}
	return ParseDiffStat(output)
}

func gitDiffStatOutput(ctx context.Context, workspacePath string) (string, error) {
	indexPath, err := gitIndexPath(ctx, workspacePath)
	if err != nil {
		return "", err
	}
	tempIndex, cleanup, err := copyGitIndex(indexPath)
	if err != nil {
		return "", err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}
	if _, err := runGitAtWithEnv(ctx, workspacePath, env, "add", "--intent-to-add", "--", "."); err != nil {
		return "", fmt.Errorf("git add intent to add: %w", err)
	}
	output, err := runGitAtWithEnv(ctx, workspacePath, env, "diff", "--stat", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git diff stat: %w", err)
	}
	return output, nil
}

func gitIndexPath(ctx context.Context, workspacePath string) (string, error) {
	output, err := runGitAt(ctx, workspacePath, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", fmt.Errorf("git index path: %w", err)
	}
	indexPath := strings.TrimSpace(output)
	if indexPath == "" {
		return "", errors.New("git index path is empty")
	}
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(workspacePath, indexPath)
	}
	return filepath.Clean(indexPath), nil
}

func copyGitIndex(indexPath string) (string, func(), error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return "", nil, fmt.Errorf("read git index: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(indexPath), "symphony-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary git index: %w", err)
	}
	tempIndex := file.Name()
	cleanup := func() {
		if err := os.Remove(tempIndex); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write temporary git index: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temporary git index: %w", err)
	}
	return tempIndex, cleanup, nil
}

func ParseDiffStat(output string) (DiffStat, error) {
	summary := diffStatSummaryLine(output)
	if summary == "" {
		return DiffStat{}, nil
	}
	if !diffStatFilesPattern.MatchString(summary) {
		return DiffStat{}, fmt.Errorf("parse git diff stat: missing file count in %q", summary)
	}

	return DiffStat{
		Files:   parseDiffStatInt(diffStatFilesPattern, summary),
		Added:   parseDiffStatInt(diffStatAddedPattern, summary),
		Removed: parseDiffStatInt(diffStatRemovedPattern, summary),
	}, nil
}

func diffStatSummaryLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func parseDiffStatInt(pattern *regexp.Regexp, input string) int {
	matches := pattern.FindStringSubmatch(input)
	if len(matches) < 2 {
		return 0
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return value
}
