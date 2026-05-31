package workspace

import (
	"context"
	"errors"
	"fmt"
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

	output, err := runGitAt(ctx, workspacePath, "diff", "--stat", "HEAD")
	if err != nil {
		return DiffStat{}, fmt.Errorf("git diff stat: %w", err)
	}
	return ParseDiffStat(output)
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
