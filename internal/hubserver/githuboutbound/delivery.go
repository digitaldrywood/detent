package githuboutbound

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) Process(ctx context.Context) (bool, error) {
	worked, _, err := s.processNext(ctx)
	return worked, err
}

func (s *Service) processNext(ctx context.Context) (bool, time.Time, error) {
	if s.deps.Backend == nil || s.deps.Repository == nil {
		return false, time.Time{}, s.deps.Disabled
	}
	var deadline time.Time
	if probe, ok := s.deps.Repository.(interface {
		GitHubOutboundDeadline(context.Context, tracker.OrganizationID) (time.Time, error)
	}); ok {
		var err error
		deadline, err = probe.GitHubOutboundDeadline(ctx, s.deps.Organization)
		if err != nil || deadline.IsZero() || deadline.After(s.deps.Now()) {
			return false, deadline, err
		}
	}
	worked, err := s.claimAndDeliver(ctx)
	if worked {
		deadline = time.Time{}
	}
	return worked, deadline, err
}

func (s *Service) claimAndDeliver(ctx context.Context) (bool, error) {
	organization := s.deps.Organization
	var row projectstore.GitHubOutbound
	err := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		var err error
		gitHubOutboxWriter, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return s.deps.Unavailable
		}
		row, err = gitHubOutboxWriter.ClaimGitHubOutbound(ctx, s.deps.Now(), s.deps.Now().Add(s.deps.OutboxProcessingTimeout), s.deps.NewLeaseID())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	callContext, stop := context.WithDeadline(ctx, row.NextAttemptAt.Add(-s.deps.OutboxProcessingTimeout/10))
	defer stop()
	desired, err := eventstore.NewContent(s.deps.Details).Receipt(callContext, row.Payload)
	result := githubcontract.OutboxStepResult{Progress: row.Progress}
	writing := false
	if err == nil {
		owner, name, valid := s.deps.SplitRepository(row.Repository)
		if !valid {
			err = errors.New("GitHub outbound repository is invalid")
		} else {
			item := githubcontract.OutboxItem{OrganizationID: organization, ID: row.ID, IdempotencyKey: row.Key, Profile: "native", RepositoryOwner: owner, RepositoryName: name, IssueNumber: int(row.IssueNumber), Kind: githubcontract.MutationKind(row.Kind), TargetNodeID: row.TargetNodeID, Desired: desired, Status: row.Status, Attempts: int(row.Attempts), CreatedAt: row.CreatedAt, Progress: row.Progress, Uncertain: row.Outcome == "uncertain", Blocked: row.Blocked}
			result, writing, err = s.executeStep(callContext, row, item)
		}
	}
	changedSource := !row.Progress.SourceUpdatedAt.IsZero() && !result.Progress.SourceUpdatedAt.IsZero() && !row.Progress.SourceUpdatedAt.Equal(result.Progress.SourceUpdatedAt)
	row.Progress, row.Observations = result.Progress, result.Observations
	row.Status = "pending"
	row.NextAttemptAt = s.deps.Now().Add(RetryDelay(1, s.deps.OutboxPollInterval, s.deps.OutboxPollInterval, s.deps.Jitter()))
	if writing {
		row.Outcome = "uncertain"
	}
	if err != nil {
		row.Status = "retrying"
		row.NextAttemptAt = s.deps.Now().Add(RetryDelay(row.Attempts, s.deps.OutboxBaseBackoff, s.deps.OutboxMaxBackoff, s.deps.Jitter()))
		if writing {
			row.Progress = githubcontract.OutboxProgress{Verification: row.Progress.Verification, SourceUpdatedAt: row.Progress.SourceUpdatedAt}
			row.Observations = nil
		}
		if githubcontract.IsRejectedOutboxWrite(err) {
			row.Outcome = "rejected"
		}
	} else {
		row.Attempts = max(0, row.Attempts-1)
		if changedSource || result.Done {
			row.Attempts = 0
		}
		if writing || result.Done {
			row.Outcome = "committed"
		}
		if result.Done {
			row.Status = "completed"
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	writeErr := s.deps.Repository.WriteTenant(cleanup, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubOutboxWriter, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return gitHubOutboxWriter.FinishGitHubOutbound(cleanup, row, s.deps.Now())
	})
	return err == nil && writeErr == nil, errors.Join(err, writeErr)
}

func (s *Service) renew(ctx context.Context, row projectstore.GitHubOutbound, writing bool) error {
	now := s.deps.Now()
	return s.deps.Repository.WriteTenant(ctx, row.Payload.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return owner.RenewGitHubOutbound(ctx, row.ID, row.LeaseID, now, now.Add(s.deps.OutboxProcessingTimeout), writing)
	})
}

func (s *Service) executeStep(parent context.Context, row projectstore.GitHubOutbound, item githubcontract.OutboxItem) (result githubcontract.OutboxStepResult, writing bool, resultErr error) {
	ctx, cancel := context.WithCancelCause(parent)
	stopped := make(chan struct{})
	joined := make(chan error, 1)
	var started atomic.Bool
	go func() {
		timer := s.deps.NewTimer(s.deps.OutboxProcessingTimeout / 3)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				joined <- nil
				return
			case <-stopped:
				joined <- nil
				return
			case <-timer.Ticks:
				renewContext, stop := context.WithTimeout(ctx, min(5*time.Second, s.deps.OutboxProcessingTimeout/3))
				err := s.renew(renewContext, row, false)
				stop()
				if err != nil {
					cancel(err)
					joined <- err
					return
				}
				timer.Reset(s.deps.OutboxProcessingTimeout / 3)
			}
		}
	}()
	defer func() {
		close(stopped)
		cancel(nil)
		resultErr = errors.Join(resultErr, <-joined)
		writing = started.Load()
	}()
	item.BeforeWrite = func(writeContext context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		started.Store(true)
		if err := writeContext.Err(); err != nil {
			return err
		}
		if err := s.renew(ctx, row, true); err != nil {
			cancel(err)
			return err
		}
		return ctx.Err()
	}
	if backend, ok := s.deps.Backend.(githubcontract.OutboxStepBackend); ok {
		result, resultErr = backend.Step(ctx, item)
		return result, false, resultErr
	}
	result.Progress = item.Progress
	if item.Uncertain {
		return result, false, githubcontract.ErrOutboxUncertain
	}
	if err := item.BeforeWrite(ctx); err != nil {
		return result, false, err
	}
	if item.Kind.Irreversible() {
		resultErr = s.deps.Backend.VerifyAndExecute(ctx, item)
	} else {
		resultErr = s.deps.Backend.Execute(ctx, item)
	}
	result.Done = resultErr == nil
	return result, false, resultErr
}
