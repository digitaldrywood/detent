package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) intakeNativeSource(ctx context.Context, source *NativeConnector, lease tracker.NativeLease, issue tracker.NativeIssue) error {
	if issue.LinkedSource == nil || issue.LinkedSource.Status == "complete" {
		return nil
	}
	if s.githubIntake == nil {
		return fmt.Errorf("runner GitHub source intake requires authenticated GitHub read access; configure the instance credential and retry: %w", ErrUnavailable)
	}
	snapshot, err := s.githubIntake(ctx, issue.LinkedSource.URL)
	if err != nil {
		return fmt.Errorf("runner GitHub source intake failed; check instance credentials, repository access or rate limit, then retry: %w", errors.Join(ErrUnavailable, err))
	}
	key, err := randomSessionID()
	if err != nil {
		return err
	}
	_, err = source.NativeClient().IntakeSource(ctx, lease.WorkItemID, tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Snapshot: snapshot})
	if err != nil {
		return fmt.Errorf("persist complete source intake before agent dispatch: %w", errors.Join(ErrUnavailable, err))
	}
	return nil
}
