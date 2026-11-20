package workspace

import (
	"context"
	"errors"
	"os/exec"
	"strings"

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
