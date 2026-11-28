package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/reviewoutput"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace/validationcmd"
)

type reviewOutputRangeKey struct{}

func WithReviewOutputRange(ctx context.Context, selection ReviewOutputRange) context.Context {
	return context.WithValue(ctx, reviewOutputRangeKey{}, selection)
}

type ReviewCommandRunner interface {
	RunReviewCommand(context.Context, Info, Issue, string) (gate.CommandResult, error)
}

type NoChangeReviewPreparer interface {
	PrepareNoChangeReview(context.Context, Info, Issue, string) (Info, Issue, func() error, error)
}

func (l *LocalGit) PrepareNoChangeReview(ctx context.Context, info Info, issue Issue, expected string) (Info, Issue, func() error, error) {
	noop := func() error { return nil }
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return info, issue, noop, nil
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return Info{}, Issue{}, noop, err
	}
	checkedOut, err := l.Head(ctx, normalized, issue)
	if err != nil {
		return Info{}, Issue{}, noop, err
	}
	checkedOut = strings.TrimSpace(checkedOut)
	if checkedOut == "" || checkedOut == expected {
		return info, issue, noop, nil
	}
	checkedOutTree, err := gitexec.Run(ctx, normalized.Path, "rev-parse", "--verify", checkedOut+"^{tree}")
	if err != nil {
		return info, issue, noop, nil
	}
	expectedTree, err := gitexec.Run(ctx, normalized.Path, "rev-parse", "--verify", expected+"^{tree}")
	if err != nil || strings.TrimSpace(checkedOutTree) == "" || strings.TrimSpace(checkedOutTree) != strings.TrimSpace(expectedTree) {
		return info, issue, noop, nil
	}
	scratch, err := os.MkdirTemp(l.root, "nochange-review-*")
	if err != nil {
		return Info{}, Issue{}, noop, fmt.Errorf("prepare no-change review scratch: %w", err)
	}
	basePath := filepath.Join(scratch, filepath.Base(normalized.Path))
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		_ = os.Remove(scratch)
		return Info{}, Issue{}, noop, err
	}
	_, addErr := runGitAt(ctx, normalized.Path, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "--detach", basePath, expected)
	if addErr != nil {
		err := rollbackNoChangeReviewWorktree(ctx, basePath, scratch, addErr, func(cleanupCtx context.Context, path string) error {
			return l.removeLandingWorktree(cleanupCtx, l.sourceRoot, path)
		})
		release()
		return Info{}, Issue{}, noop, err
	}
	release()
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		rel, lockErr := l.acquireSourceOperation(cleanupCtx)
		if lockErr != nil {
			return lockErr
		}
		defer rel()
		if err := l.removeLandingWorktree(cleanupCtx, l.sourceRoot, basePath); err != nil {
			return err
		}
		if err := os.Remove(scratch); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	reviewInfo := normalized
	reviewInfo.Path = basePath
	reviewInfo.Branch = ""
	reviewIssue := issue
	reviewIssue.preparedValidationPath = basePath
	return reviewInfo, reviewIssue, cleanup, nil
}

func rollbackNoChangeReviewWorktree(ctx context.Context, basePath, scratch string, addErr error, remove func(context.Context, string) error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var cleanupErr error
	if rmErr := remove(cleanupCtx, basePath); rmErr != nil {
		cleanupErr = rmErr
	} else if rmDirErr := os.Remove(scratch); rmDirErr != nil && !errors.Is(rmDirErr, os.ErrNotExist) {
		cleanupErr = rmDirErr
	}
	return errors.Join(fmt.Errorf("checkout no-change review base: %w", addErr), cleanupErr)
}

func (l *LocalGit) ReviewCommandEnvironment(ctx context.Context, info Info, issue Issue, validation string) gate.CheckEnvironment {
	return l.ReviewCommandEvidence(ctx, info, issue, validation).Environment
}

func (l *LocalGit) ReviewCommandEvidence(ctx context.Context, info Info, issue Issue, validation string) *gate.CommandEvidence {
	ctx = WithAttemptWorkspace(ctx, issue.AttemptWorkspace)
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	command := commandshell.Command(ctx, "", l.hooks.Shell)
	command.Dir = normalized.Path
	command.Env = hookEnv(normalized, issue, l.hooks.StripGitHubTokens)
	l.applyValidationGoBudget(ctx, command)
	return validationInputs(command).Evidence(ctx, validation)
}

func validationInputs(cmd *exec.Cmd) validationcmd.Inputs {
	return validationcmd.Inputs{Dir: cmd.Dir, Path: cmd.Path, Args: cmd.Args, Env: cmd.Env}
}

func (l *LocalGit) applyValidationGoBudget(ctx context.Context, command *exec.Cmd) {
	variables, err := validationcmd.GoBudget(ctx, command.Environ())
	if err != nil {
		l.logger.Warn("go build budget unavailable; validation Go commands run unqueued", "error", err)
		return
	}
	procgroup.SetEnvironment(command, procgroup.Environment{Variables: variables})
}

func (l *LocalGit) RunReviewCommand(ctx context.Context, info Info, issue Issue, command string) (result gate.CommandResult, resultErr error) {
	defer func() { gate.ObservePipelineCommand(ctx, &result) }()
	if strings.TrimSpace(command) == "" || strings.TrimSpace(issue.PullRequestHeadSHA) == "" {
		return gate.CommandResult{}, errors.New("review command requires a command and immutable head")
	}
	ctx = WithAttemptWorkspace(ctx, issue.AttemptWorkspace)
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return gate.CommandResult{}, err
	}
	return validationcmd.RunReview(ctx, normalized.Path, issue.PullRequestHeadSHA, command, func(ctx context.Context, command string) (gate.CommandResult, error) {
		return l.runValidationCommand(ctx, normalized, issue, command)
	})
}

func (l *LocalGit) RunBaseCommand(ctx context.Context, info Info, issue Issue, base, command string) (gate.CommandResult, error) {
	return validationcmd.RunBase(ctx, info.Path, base, func() (string, error) {
		return PrepareWorkerScratch(ctx, info.Path)
	}, func(path string) (gate.CommandResult, error) {
		info.Path, issue.PullRequestHeadSHA = path, base
		return l.runValidationCommand(ctx, info, issue, command)
	})
}

func AttributeCommandFailure(result *gate.CommandResult, base string, reproduction gate.CommandResult, err error) {
	validationcmd.AttributeCommandFailure(result, base, reproduction, err)
}

const ReviewSourcePageBytes = reviewoutput.SourcePageBytes

type ReviewOutputRange = reviewoutput.Range
type reviewOutputCapture = reviewoutput.Capture
