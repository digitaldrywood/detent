package workspace

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

const ReviewSourcePageBytes = 16 * 1024

type ReviewOutputRange struct {
	Offset int64
	Length int
}

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
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	command := commandshell.Command(ctx, "", l.hooks.Shell)
	command.Dir = normalized.Path
	command.Env = hookEnv(normalized, issue, l.hooks.StripGitHubTokens)
	l.applyValidationGoBudget(ctx, command)
	return reviewCommandEvidence(ctx, command, validation)
}

func (l *LocalGit) applyValidationGoBudget(ctx context.Context, command *exec.Cmd) {
	inherited := make(map[string]string)
	for _, entry := range command.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			inherited[key] = value
		}
	}
	variables, err := gobudget.FromContext(ctx).Environment(inherited["GOFLAGS"])
	if err != nil {
		l.logger.Warn("go build budget unavailable; validation Go commands run unqueued", "error", err)
		return
	}
	if _, explicit := inherited["GOMAXPROCS"]; explicit {
		delete(variables, "GOMAXPROCS")
	}
	procgroup.SetEnvironment(command, procgroup.Environment{Variables: variables})
}

func directGoValidation(validation string) bool {
	return validation == "go version"
}

func (l *LocalGit) RunReviewCommand(ctx context.Context, info Info, issue Issue, command string) (result gate.CommandResult, resultErr error) {
	defer func() { gate.ObservePipelineCommand(ctx, &result) }()
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
	result, err = l.runValidationCommand(ctx, normalized, issue, command)
	result.HeadSHA = strings.TrimSpace(head)
	result.TreeSHA = strings.TrimSpace(tree)
	result.ReceiptID = result.PipelineTiming("").ReceiptID
	const maxOutputBytes = 64 * 1024
	output, truncated := runtimeoutput.Diagnostic(result.Output, maxOutputBytes)
	result.Output = output
	result.OutputTruncated = result.OutputTruncated || truncated
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
