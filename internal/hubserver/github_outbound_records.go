package hubserver

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) processGitHubOutboundRecord(ctx context.Context) (bool, error) {
	if s.config.OutboxBackend == nil || s.config.TenantRepository == nil {
		return false, ErrOutboxDisabled
	}
	organization := s.database.hostedOrganization
	var row projectstore.GitHubOutbound
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		var err error
		gitHubOutboxWriter, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		row, err = gitHubOutboxWriter.ClaimGitHubOutbound(ctx, s.config.now(), s.config.now().Add(s.config.OutboxProcessingTimeout), s.config.newLeaseID())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	callContext, stop := context.WithDeadline(ctx, row.NextAttemptAt.Add(-s.config.OutboxProcessingTimeout/10))
	defer stop()
	desired, err := eventstore.NewContent(s.runDetailStore()).Receipt(callContext, row.Payload)
	result := OutboxStepResult{Progress: row.Progress}
	writing := false
	if err == nil {
		owner, name, valid := splitRepositoryFullName(row.Repository)
		if !valid {
			err = errors.New("GitHub outbound repository is invalid")
		} else {
			item := OutboxItem{OrganizationID: organization, ID: row.ID, IdempotencyKey: row.Key, Profile: "native", RepositoryOwner: owner, RepositoryName: name, IssueNumber: int(row.IssueNumber), Kind: MutationKind(row.Kind), TargetNodeID: row.TargetNodeID, Desired: desired, Status: row.Status, Attempts: int(row.Attempts), CreatedAt: row.CreatedAt, Progress: row.Progress, Uncertain: row.Outcome == "uncertain", Blocked: row.Blocked}
			result, writing, err = s.executeGitHubOutboundStep(callContext, row, item)
		}
	}
	changedSource := !row.Progress.SourceUpdatedAt.IsZero() && !result.Progress.SourceUpdatedAt.IsZero() && !row.Progress.SourceUpdatedAt.Equal(result.Progress.SourceUpdatedAt)
	row.Progress, row.Observations = result.Progress, result.Observations
	row.Status = "pending"
	row.NextAttemptAt = s.config.now().Add(gitHubRetryDelay(1, s.config.OutboxPollInterval, s.config.OutboxPollInterval, s.config.jitter()))
	if writing {
		row.Outcome = "uncertain"
	}
	if err != nil {
		row.Status = "retrying"
		row.NextAttemptAt = s.config.now().Add(gitHubRetryDelay(row.Attempts, s.config.OutboxBaseBackoff, s.config.OutboxMaxBackoff, s.config.jitter()))
		if writing {
			row.Progress = OutboxProgress{Verification: row.Progress.Verification, SourceUpdatedAt: row.Progress.SourceUpdatedAt}
			row.Observations = nil
		}
		if isRejectedOutboxWrite(err) {
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
	writeErr := s.config.TenantRepository.WriteTenant(cleanup, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubOutboxWriter, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return gitHubOutboxWriter.FinishGitHubOutbound(cleanup, row, s.config.now())
	})
	return err == nil && writeErr == nil, errors.Join(err, writeErr)
}

func (s *Service) renewGitHubOutbound(ctx context.Context, row projectstore.GitHubOutbound, writing bool) error {
	now := s.config.now()
	return s.config.TenantRepository.WriteTenant(ctx, row.Payload.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return owner.RenewGitHubOutbound(ctx, row.ID, row.LeaseID, now, now.Add(s.config.OutboxProcessingTimeout), writing)
	})
}

func (s *Service) executeGitHubOutboundStep(parent context.Context, row projectstore.GitHubOutbound, item OutboxItem) (result OutboxStepResult, writing bool, resultErr error) {
	ctx, cancel := context.WithCancelCause(parent)
	stopped := make(chan struct{})
	joined := make(chan error, 1)
	var started atomic.Bool
	go func() {
		timer := s.config.newOutboxTimer(s.config.OutboxProcessingTimeout / 3)
		defer timer.stop()
		for {
			select {
			case <-ctx.Done():
				joined <- nil
				return
			case <-stopped:
				joined <- nil
				return
			case <-timer.ticks:
				renewContext, stop := context.WithTimeout(ctx, min(5*time.Second, s.config.OutboxProcessingTimeout/3))
				err := s.renewGitHubOutbound(renewContext, row, false)
				stop()
				if err != nil {
					cancel(err)
					joined <- err
					return
				}
				timer.reset(s.config.OutboxProcessingTimeout / 3)
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
		if err := s.renewGitHubOutbound(ctx, row, true); err != nil {
			cancel(err)
			return err
		}
		return ctx.Err()
	}
	if backend, ok := s.config.OutboxBackend.(OutboxStepBackend); ok {
		result, resultErr = backend.Step(ctx, item)
		return result, false, resultErr
	}
	result.Progress = item.Progress
	if item.Uncertain {
		return result, false, ErrOutboxUncertain
	}
	if err := item.BeforeWrite(ctx); err != nil {
		return result, false, err
	}
	if item.Kind.irreversible() {
		resultErr = s.config.OutboxBackend.VerifyAndExecute(ctx, item)
	} else {
		resultErr = s.config.OutboxBackend.Execute(ctx, item)
	}
	result.Done = resultErr == nil
	return result, false, resultErr
}
