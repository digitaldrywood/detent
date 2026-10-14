package workspace

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

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
	return reviewCommandEvidence(ctx, command, validation)
}

func directGoValidation(validation string) bool {
	args := strings.Fields(validation)
	if len(args) < 2 || args[0] != "go" || strings.ContainsAny(validation, "\\\"'`$;&|<>\n\r()%!^") {
		return false
	}
	switch args[1] {
	case "build", "test", "vet", "version":
	default:
		return false
	}
	if args[1] == "version" && len(args) != 2 {
		return false
	}
	return reusableGoValidationFlags(args[2:])
}

func reusableGoValidationFlags(args []string) bool {
	for _, arg := range args {
		if strings.ContainsAny(arg, "\"'") {
			return false
		}
		if strings.HasPrefix(arg, "--") {
			arg = "-" + strings.TrimLeft(arg, "-")
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(arg, "=")
		name = strings.TrimPrefix(name, "-test.")
		name = strings.TrimPrefix(name, "-")
		switch name {
		case "a", "n", "x", "v", "race", "msan", "asan", "trimpath", "work", "p", "buildvcs", "buildmode", "tags", "mod", "modcacherw", "installsuffix", "c", "short", "json", "run", "bench", "benchtime", "benchmem", "count", "cpu", "failfast", "fullpath", "list", "parallel", "shuffle", "skip", "timeout", "vet":
		default:
			return false
		}
	}
	return true
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
	const truncatedOutputMarker = "[earlier output truncated]\n"
	result.Output = strings.ToValidUTF8(result.Output, "�")
	if len(result.Output) > maxOutputBytes {
		start := len(result.Output) - maxOutputBytes + len(truncatedOutputMarker)
		for !utf8.RuneStart(result.Output[start]) {
			start++
		}
		result.Output = truncatedOutputMarker + result.Output[start:]
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
