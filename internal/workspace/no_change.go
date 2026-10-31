package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type CurrentBaseProvider interface {
	CurrentBase(context.Context, Info, Issue) (string, error)
}

func (l *LocalGit) CurrentBase(ctx context.Context, info Info, issue Issue) (string, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(issue.ProgressBaseRef)
	if branch == "" {
		branch, err = remoteDefaultBranch(ctx, normalized.Path, defaultGitRemote)
		if err != nil {
			return "", fmt.Errorf("resolve completion base branch: %w", err)
		}
	}
	head, exists, err := remoteBranchHead(ctx, normalized.Path, defaultGitRemote, branch)
	if err != nil {
		return "", fmt.Errorf("read completion base: %w", err)
	}
	if !exists || !validLandingHead(head) {
		return "", errors.New("completion base is unavailable")
	}
	return head, nil
}
