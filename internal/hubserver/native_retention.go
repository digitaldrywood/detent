package hubserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	commandReceiptRetention  = receiptstore.ReplayRetention
	commandReceiptPruneBatch = 5000
	hostedAuditRetention     = 30 * 24 * time.Hour
	nativeCleanupInterval    = time.Hour
)

type nativeRetentionStage struct {
	name string
	run  func(context.Context) error
}

func (s *Service) maintainNativeBackfills(ctx context.Context) error {
	return s.runNativeRetentionStages(ctx, []nativeRetentionStage{
		{"populate Change columns", s.populateChangeColumns},
		{"populate claim facts", s.populateClaimRecoveryProjections},
		{"expire request metrics", func(ctx context.Context) error { return s.expireRequestMetrics(ctx, s.config.now()) }},
	})
}

func (s *Service) populateClaimRecoveryProjections(ctx context.Context) error {
	owner, ok := s.config.TenantRepository.(interface {
		BackfillChangeRecoveryProjections(context.Context, tracker.OrganizationID, *eventstore.Content, int) (int, error)
	})
	if !ok {
		return workItemStorageUnavailable()
	}
	_, err := owner.BackfillChangeRecoveryProjections(ctx, tracker.OrganizationID(s.appOrganizationID()), eventstore.NewContent(s.runDetailStore()), corestore.MaxPageSize)
	return err
}

func (s *Service) maintainNativeRetention(ctx context.Context, now time.Time) error {
	backfillErr := s.maintainNativeBackfills(ctx)
	cleanupErr := s.runNativeRetentionStages(ctx, []nativeRetentionStage{
		{"expire native records", func(ctx context.Context) error { return s.expireNativeRecords(ctx, now) }},
		{"compact conversation events", func(ctx context.Context) error {
			if s.conversations == nil {
				return nil
			}
			_, err := s.conversations.compactEventsPG(ctx, tracker.OrganizationID(s.appOrganizationID()), now)
			return err
		}},
	})
	return errors.Join(backfillErr, cleanupErr)
}

func (s *Service) runNativeRetentionStages(ctx context.Context, stages []nativeRetentionStage) error {
	withTimeout := s.config.retentionTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	var failures []error
	for _, stage := range stages {
		stageCtx, cancel := withTimeout(ctx, backgroundStorageBudget)
		err := stage.run(stageCtx)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", stage.name, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) expireNativeRecords(ctx context.Context, now time.Time) error {
	if s.config.TenantRepository == nil {
		return workItemStorageUnavailable()
	}
	organization := tracker.OrganizationID(s.appOrganizationID())
	if organization == "" {
		return errors.New("runner host metric retention requires an organization")
	}
	cutoff := now.UTC().Truncate(time.Hour).Add(-720 * time.Hour)
	if err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := w.PruneRunnerHostMetrics(ctx, cutoff, runnerstore.MaxRunnerHostPruneBatch); err != nil {
			return err
		}
		if _, err := w.PruneExpiredArtifactReadGrants(ctx, now, artifactstore.MaxReadGrantPruneBatch); err != nil {
			return err
		}
		if _, err := w.PruneExpiredArtifactUploadAuthorizations(ctx, artifactstore.MaxUploadAuthorizationPruneBatch); err != nil {
			return err
		}
		writer, ok := w.(corestore.DuplicateChallengeWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		_, err := writer.PruneDuplicateChallenges(ctx, now, commandReceiptPruneBatch)
		return err
	}); err != nil {
		return fmt.Errorf("expire PostgreSQL native records: %w", err)
	}
	return s.retainCommandPayloads(ctx, now)
}
