package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func (s *Service) importSpriteUsageRecords(ctx context.Context, scope nativeScope, request spriteUsageRequest) ([]byte, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	ctx = isolateContentContext(s.runDetailContext(ctx))
	if s.runDetailStore() == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	const operation = "sprite_usage.import"
	usageReceiptID := newNativeID("sprite_usage")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var replay *eventstore.Receipt
		expected := make([]bool, len(request.Observations))
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			previous, err := r.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				replay = &previous
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			revisions := make(map[billingstore.CostSource]int64)
			for i, o := range request.Observations {
				key := billingstore.CostSource{Provider: o.Provider, ProviderAccount: o.ProviderAccount, SourceID: o.SourceID, From: o.From, To: o.To}
				revision, found := revisions[key]
				if !found {
					current, err := r.BillingCostRevision(ctx, key)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					revision = current.Revision
				}
				expected[i] = o.Revision > revision
				revisions[key] = max(o.Revision, revision)
			}
			return nil
		})
		if err != nil {
			return nil, projectRecordError(err)
		}
		if replay != nil {
			if replay.RequestHash != hash {
				return nil, nativeConflict(0)
			}
			return content.Receipt(ctx, *replay)
		}
		records := make([]billingstore.CostObservation, len(request.Observations))
		recorded := 0
		for i, o := range request.Observations {
			raw, err := json.Marshal(o)
			if err != nil {
				return nil, err
			}
			ref, err := s.runDetailStore().StoreDetail(ctx, string(scope.organization), raw)
			if err != nil {
				return nil, err
			}
			records[i] = billingstore.CostObservation{OrganizationID: scope.organization, ProjectID: scope.project, Observation: o, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: s.config.now()}
			if expected[i] {
				recorded++
			}
		}
		raw, err := json.Marshal(struct {
			Recorded int `json:"recorded"`
		}{recorded})
		if err != nil {
			return nil, err
		}
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
		if err != nil {
			return nil, err
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			previous, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				replay = &previous
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if s.database.billingRepository != nil {
				if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
					return err
				}
			}
			results, err := w.RecordBillingCosts(ctx, records)
			if err != nil {
				return err
			}
			for i, applied := range results {
				if applied != expected[i] {
					return corestore.ErrSerializationFailure
				}
			}
			if err := s.applyRunnerTenantUsage(ctx, w, receipt.CreatedAt, usageReceiptID, false, false); err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if errors.Is(err, mutation.ErrConflict) {
			return nil, nativeConflict(0)
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		if replay != nil {
			if replay.RequestHash != hash {
				return nil, nativeConflict(0)
			}
			return content.Receipt(ctx, *replay)
		}
		return raw, nil
	}
}

func (s *Service) prepareRunnerCostRecords(ctx context.Context, scope nativeScope, request tracker.NativeRunEvent, facts runnerUsageFacts, now time.Time) ([]billingstore.CostObservation, error) {
	if request.Type != "run.finished" || len(request.Data.Usage) == 0 {
		return nil, nil
	}
	observations, err := buildRunnerUsageObservations(request, s.usagePrices(), facts)
	if err != nil {
		return nil, err
	}
	records := make([]billingstore.CostObservation, 0, len(observations))
	for i := range observations {
		observation := &observations[i].observation
		if err := normalizeCostObservation(observation, now); err != nil {
			return nil, err
		}
		for _, prior := range observations[:i] {
			previous := prior.observation
			if observation.Provider == previous.Provider && observation.Metric == previous.Metric && observation.From.Before(previous.To) && previous.From.Before(observation.To) {
				return nil, nativePublicInvalid("Overlapping usage must correct the original source period instead of adding another source")
			}
		}
		raw, err := json.Marshal(observation)
		if err != nil {
			return nil, err
		}
		ref, err := s.runDetailStore().StoreDetail(ctx, string(scope.organization), raw)
		if err != nil {
			return nil, err
		}
		if err := validateNativeDetailReference(ctx, s.runDetailStore(), scope.organization, ref, raw); err != nil {
			return nil, err
		}
		records = append(records, billingstore.CostObservation{OrganizationID: scope.organization, ProjectID: scope.project, Observation: *observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now})
	}
	return records, nil
}
