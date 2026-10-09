package workspace

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
)

type ReviewCommandRunner interface {
	RunReviewCommand(context.Context, Info, Issue, string) (gate.CommandResult, error)
}

func (l *LocalGit) RunReviewCommand(ctx context.Context, info Info, issue Issue, command string) (gate.CommandResult, error) {
	if strings.TrimSpace(command) == "" || strings.TrimSpace(issue.PullRequestHeadSHA) == "" {
		return gate.CommandResult{}, errors.New("review command requires a command and immutable head")
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return gate.CommandResult{}, err
	}
	head, err := l.Head(ctx, normalized, issue)
	if err != nil {
		return gate.CommandResult{}, err
	}
	if strings.TrimSpace(head) != issue.PullRequestHeadSHA {
		return gate.CommandResult{}, errors.New("review command head differs from the reviewed version")
	}
	if err := l.VerifyReviewTree(ctx, normalized, issue); err != nil {
		return gate.CommandResult{}, err
	}
	tree, err := runGitAt(ctx, normalized.Path, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return gate.CommandResult{}, err
	}
	result, err := l.runValidationCommand(ctx, normalized, issue, command)
	result.HeadSHA = strings.TrimSpace(head)
	result.TreeSHA = strings.TrimSpace(tree)
	const maxOutputBytes = 64 * 1024
	const truncatedOutputMarker = "[earlier output truncated]\n"
	if len(result.Output) > maxOutputBytes {
		result.Output = truncatedOutputMarker + result.Output[len(result.Output)-maxOutputBytes+len(truncatedOutputMarker):]
		result.OutputTruncated = true
	}
	if err != nil {
		return result, err
	}
	current, err := l.Head(ctx, normalized, issue)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(current) != result.HeadSHA {
		return result, errors.New("review command changed the reviewed head")
	}
	return result, l.VerifyReviewTree(ctx, normalized, issue)
}
