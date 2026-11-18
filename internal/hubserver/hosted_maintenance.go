package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/validation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func maintenanceFeature(name string, step func(context.Context, int) (int, error)) hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: name, Bytes: hubmaintenance.ByteLimit / hubmaintenance.TenantLimit, Step: func(ctx context.Context, _ hubmaintenance.Tenant, b hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		used, err := step(ctx, b.Items)
		return hubmaintenance.Outcome{Items: used, Progress: used > 0}, err
	}}
}

func (s *Service) cursorMaintenance(name string, step func(context.Context, *postgres.MaintenanceCursor, int) (int, error)) hubmaintenance.Feature {
	return maintenanceFeature(name, func(ctx context.Context, limit int) (int, error) {
		tenant := s.MaintenanceTenant()
		organization := tracker.OrganizationID(tenant.Organization)
		cursor, err := s.database.postgres.MaintenanceCursor(ctx, organization, tenant.Generation, name)
		if err != nil {
			return 0, err
		}
		prior := cursor
		used, err := step(ctx, &cursor, limit)
		if prior != cursor {
			err = errors.Join(err, s.database.postgres.SetMaintenanceCursor(ctx, organization, tenant.Generation, name, cursor))
		}
		return used, err
	})
}

func (s *Service) SharedMaintenanceFeatures() []hubmaintenance.Feature {
	organization := s.config.runtimeTenant
	now := s.config.Clock
	features := append(s.ReadModelMaintenanceFeatures(),
		s.cursorMaintenance("change-reviews", func(ctx context.Context, c *postgres.MaintenanceCursor, limit int) (int, error) {
			n, next, err := s.database.postgres.BackfillCurrentChangeReviewsAfter(ctx, organization, eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore())), limit, c.Key)
			if next != "" || err == nil {
				c.Key = next
			}
			return n, err
		}),
		s.cursorMaintenance("checkpoint-sources", func(ctx context.Context, c *postgres.MaintenanceCursor, limit int) (int, error) {
			next, err := s.populateCheckpointSourcePage(ctx, organization, c.Key, limit)
			if next != "" || err == nil {
				c.Key = next
			}
			return limit, err
		}),
		maintenanceFeature("claim-projections", func(ctx context.Context, limit int) (int, error) {
			return s.database.postgres.BackfillChangeRecoveryProjections(ctx, organization, eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore())), limit)
		}),
		maintenanceFeature("team-unread", func(ctx context.Context, limit int) (int, error) {
			_, n, err := s.threadsService().AdvanceUnreadPage(ctx, limit-1)
			return n + 1, err
		}),
		maintenanceFeature("origin-identities", func(ctx context.Context, _ int) (int, error) {
			processed, err := s.refreshNativeIdentity(ctx)
			if processed {
				return 1, err
			}
			return 0, err
		}),
		maintenanceFeature("test-receipts", func(ctx context.Context, limit int) (int, error) {
			used := 0
			err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
				at := now()
				receipts, attestations, err := w.PruneGoTestReceipts(ctx, at.Add(-validation.ReceiptRetention), at, limit/2)
				used = int(receipts + attestations)
				return err
			})
			return used, err
		}),
		maintenanceFeature("closed-work-items", func(ctx context.Context, limit int) (int, error) {
			return min(4, limit), s.retainClosedWorkItems(ctx, organization, now())
		}),
		maintenanceFeature("command-payloads", func(ctx context.Context, limit int) (int, error) {
			return limit, s.accessReceiptsService().RetainPage(ctx, now(), limit)
		}),
		maintenanceFeature("issue-archives", func(ctx context.Context, limit int) (int, error) {
			if s.storageArchiveContent() == nil {
				return 0, nil
			}
			return limit, s.archiveNativeIssuePage(ctx, organization, now(), 1, (limit-1)/2)
		}),
		maintenanceFeature("archive-sections", func(ctx context.Context, limit int) (int, error) {
			if s.storageArchiveContent() == nil {
				return 0, nil
			}
			return limit, s.retireStorageArchivePage(ctx, organization, now(), 1, limit-1)
		}),
		s.cursorMaintenance("detail-reclamation", func(ctx context.Context, c *postgres.MaintenanceCursor, limit int) (int, error) {
			if s.database.detailObjects == nil {
				return 0, nil
			}
			next, _, err := s.database.detailObjects.ReclaimPage(ctx, organization, now().Add(-attachment.OrphanTTL), c.Key, limit)
			if err == nil {
				c.Key = next
			}
			return limit, err
		}),
		maintenanceFeature("request-metric-retention", func(ctx context.Context, limit int) (int, error) {
			return limit, s.database.postgres.RetainRequestMetricPage(ctx, organization, now(), limit/4)
		}),
		maintenanceFeature("request-metric-flush", func(ctx context.Context, limit int) (int, error) {
			if s.requestMetrics == nil {
				return 0, nil
			}
			return limit, s.requestMetrics.FlushPage(ctx, now(), limit)
		}),
		maintenanceFeature("request-usage-retention", func(ctx context.Context, limit int) (int, error) {
			if s.requestMetrics == nil {
				return 0, nil
			}
			return limit, s.requestMetrics.RetainUsage(ctx, now(), limit/2)
		}),
		s.secretMaintenance(),
		maintenanceFeature("github-outbox", func(ctx context.Context, _ int) (int, error) {
			if s.outbox == nil {
				return 0, nil
			}
			processed, err := s.ProcessOutbox(ctx)
			if processed {
				return 1, err
			}
			return 0, err
		}),
	)
	billing := maintenanceFeature("billing-reconciliation", s.maintainHostedBilling)
	billing.Lead = true
	billing.Bytes = hubmaintenance.ByteLimit / (2 * hubmaintenance.TenantLimit)
	features = append([]hubmaintenance.Feature{billing}, features...)
	features = append(features, s.retentionFeatures()...)
	features = append(features, s.sessionMaintenanceFeatures()...)
	for i := range features {
		step := features[i].Step
		features[i].Step = func(ctx context.Context, t hubmaintenance.Tenant, b hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			if err := hubmaintenance.RequireTenant(ctx, t); err != nil {
				return hubmaintenance.Outcome{}, err
			}
			if t != s.MaintenanceTenant() {
				return hubmaintenance.Outcome{}, corestore.ErrTenantMismatch
			}
			if b.Items < hubmaintenance.MinItems || b.Items > hubmaintenance.ItemLimit {
				return hubmaintenance.Outcome{}, corestore.ErrInvalidPage
			}
			return step(s.runDetailContext(ctx), t, b)
		}
	}
	return features
}

func (s *Service) retentionFeatures() []hubmaintenance.Feature {
	type prune func(context.Context, tenantstore.Writer, time.Time, int) (int, error)
	feature := func(name string, run prune) hubmaintenance.Feature {
		return maintenanceFeature(name, func(ctx context.Context, limit int) (int, error) {
			used := 0
			err := s.config.TenantRepository.WriteTenant(ctx, s.config.runtimeTenant, corestore.Background, func(w tenantstore.Writer) error {
				var err error
				used, err = run(ctx, w, s.config.Clock(), limit)
				return err
			})
			return used, err
		})
	}
	return []hubmaintenance.Feature{
		feature("legacy-read-audits", func(ctx context.Context, w tenantstore.Writer, _ time.Time, limit int) (int, error) {
			return w.PruneAccessAuditReads(ctx, limit)
		}),
		feature("expired-access-audits", func(ctx context.Context, w tenantstore.Writer, now time.Time, limit int) (int, error) {
			return w.PruneAccessAuditsBefore(ctx, now.Add(-hostedAuditRetention), limit)
		}),
		feature("runner-host-metrics", func(ctx context.Context, w tenantstore.Writer, now time.Time, limit int) (int, error) {
			return limit, w.PruneRunnerHostMetrics(ctx, now.UTC().Truncate(time.Hour).Add(-720*time.Hour), limit)
		}),
		feature("artifact-read-grants", func(ctx context.Context, w tenantstore.Writer, now time.Time, limit int) (int, error) {
			n, err := w.PruneExpiredArtifactReadGrants(ctx, now, limit)
			return int(n), err
		}),
		feature("artifact-upload-authorizations", func(ctx context.Context, w tenantstore.Writer, _ time.Time, limit int) (int, error) {
			n, err := w.PruneExpiredArtifactUploadAuthorizations(ctx, limit)
			return int(n), err
		}),
		feature("terminal-turn-batches", func(ctx context.Context, w tenantstore.Writer, now time.Time, limit int) (int, error) {
			n, err := w.PruneTerminalTurnBatches(ctx, now, limit)
			return int(n), err
		}),
		feature("duplicate-challenges", func(ctx context.Context, w tenantstore.Writer, now time.Time, limit int) (int, error) {
			owner, ok := w.(corestore.DuplicateChallengeWriter)
			if !ok {
				return 0, hubapi.NativeWorkItemStorageUnavailable()
			}
			n, err := owner.PruneDuplicateChallenges(ctx, now, limit)
			return int(n), err
		}),
	}
}

func (s *Service) secretMaintenance() hubmaintenance.Feature {
	return s.cursorMaintenance("secret-rewrap", func(ctx context.Context, c *postgres.MaintenanceCursor, _ int) (int, error) {
		next, processed, err := s.MaintainSecret(ctx, s.config.runtimeTenant, SecretCursor{ProjectID: tracker.ProjectID(c.Project), Kind: c.Kind})
		c.Project, c.Kind = string(next.ProjectID), next.Kind
		if processed {
			return 1, err
		}
		return 0, err
	})
}

func (h *HostedRuntime) MaintainSecret(ctx context.Context, org tracker.OrganizationID, generation int64, cfg Config) (hubmaintenance.Outcome, error) {
	tenant := hubmaintenance.Tenant{Organization: string(org), Generation: generation}
	if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
		return hubmaintenance.Outcome{}, err
	}
	if !cfg.Hosted.BindsSharedOrganization(string(org)) || cfg.Hosted.SharedEntry.Generation != generation {
		return hubmaintenance.Outcome{}, ErrHostedDatabaseBinding
	}
	db, err := h.tenantDatabase(org, generation)
	if err != nil {
		return hubmaintenance.Outcome{}, err
	}
	defer db.Close()
	cfg = cfg.normalized()
	cfg.runtimeHost = h
	cfg.runtimeTenant = org
	cfg.AccessRepository = db.postgres
	service := &Service{config: cfg, database: db}
	if err := service.validateAccessHostedBinding(ctx); err != nil {
		return hubmaintenance.Outcome{}, err
	}
	return service.secretMaintenance().Step(ctx, tenant, hubmaintenance.Budget{Items: 1})
}
