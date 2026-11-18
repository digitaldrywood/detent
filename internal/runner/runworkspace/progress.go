package runworkspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var ErrWorkerProcessReap = errors.New("worker process reap failed")

func WithHooks(ctx context.Context) (context.Context, func(*error)) {
	ctx, stop := workspace.WithHookProcesses(ctx)
	return ctx, func(primary *error) {
		if err := stop(); err != nil {
			if *primary != nil {
				*primary = errors.Join(*primary, err)
				return
			}
			*primary = fmt.Errorf("%w: %w", ErrWorkerProcessReap, err)
		}
	}
}

type SourceRecovery interface {
	RecoverChangeSource(context.Context) (workspace.ChangeSource, error)
}

func PrepareProgress(ctx context.Context, backend workspace.Backend, issue workspace.Issue, source SourceRecovery, evidence progresscheck.Request) (workspace.Info, func(*error), error) {
	issue.ReviewVersionID = "progress-" + evidence.SourceSHA
	issue.PullRequestHeadSHA = evidence.SourceSHA
	issue.BaseRef = evidence.SourceSHA
	if source != nil && evidence.SourceSHA != "" {
		recovered, err := source.RecoverChangeSource(ctx)
		if err != nil {
			return workspace.Info{}, nil, fmt.Errorf("recover progress source: %w", err)
		}
		if recovered.Version.HeadSHA != "" {
			if recovered.Version.HeadSHA != evidence.SourceSHA || recovered.Version.BaseSHA != evidence.BaseSHA {
				return workspace.Info{}, nil, errors.New("progress source differs from supplied evidence")
			}
			issue.Source = &recovered
		}
	}
	if issue.Source == nil || issue.Source.Version.Source == nil {
		return workspace.Info{}, func(*error) {}, nil
	}
	release := func() {}
	if usage, ok := backend.(workspace.Usage); ok {
		var err error
		release, err = usage.Use(ctx, issue)
		if err != nil {
			return workspace.Info{}, nil, err
		}
	}
	ctx, stopHooks := WithHooks(ctx)
	finish := func(primary *error) {
		defer release()
		stopHooks(primary)
	}
	info, err := backend.Create(ctx, issue)
	if err == nil && evidence.SourceSHA != "" {
		if seeder, ok := backend.(workspace.ReviewHeadSeeder); ok {
			err = seeder.SeedReviewHead(ctx, info, issue)
		}
		if err == nil {
			if provider, ok := backend.(workspace.HeadProvider); ok {
				var head string
				head, err = provider.Head(ctx, info, issue)
				if err == nil && strings.TrimSpace(head) != evidence.SourceSHA {
					err = errors.New("progress workspace head differs from supplied source")
				}
			} else {
				err = errors.New("progress workspace cannot verify pinned head")
			}
		}
		if err == nil {
			if verifier, ok := backend.(workspace.ReviewTreeVerifier); ok {
				err = verifier.VerifyReviewTree(ctx, info, issue)
			} else {
				err = errors.New("progress workspace cannot verify pinned source")
			}
		}
	}
	if err != nil {
		finish(&err)
		return workspace.Info{}, nil, fmt.Errorf("prepare progress workspace: %w", err)
	}
	return info, finish, nil
}
