package usagecosts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) ImportRecords(ctx context.Context, scope hubapi.Scope, request SpriteRequest) ([]byte, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.PublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	ctx = s.deps.ContentContext(ctx)
	if s.deps.Details == nil {
		return nil, hubapi.RunEventStorageUnavailable()
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.deps.Details)
	const operation = "sprite_usage.import"
	usageReceiptID := hubapi.NewID("sprite_usage")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var replay *eventstore.Receipt
		expected := make([]bool, len(request.Observations))
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := scope.Authorize(ctx, r, true); err != nil {
				return err
			}
			previous, err := r.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
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
			return nil, s.deps.RecordError(err)
		}
		if replay != nil {
			if replay.RequestHash != hash {
				return nil, hubapi.Conflict(0)
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
			ref, err := s.deps.Details.StoreDetail(ctx, string(scope.Organization), raw)
			if err != nil {
				return nil, err
			}
			records[i] = billingstore.CostObservation{OrganizationID: scope.Organization, ProjectID: scope.Project, Observation: o, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: s.deps.Now()}
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
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.deps.Now()}, raw)
		if err != nil {
			return nil, err
		}
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := scope.Authorize(ctx, w, true); err != nil {
				return err
			}
			previous, err := w.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
			if err == nil {
				replay = &previous
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if s.deps.RequireCollaboration != nil {
				if err := s.deps.RequireCollaboration(ctx, w, s.deps.Now()); err != nil {
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
			if err := s.deps.AccountUsage(ctx, w, receipt.CreatedAt, usageReceiptID); err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if errors.Is(err, billingstore.ErrCostOverlap) {
			return nil, hubapi.PublicInvalid("Overlapping usage must correct the original source period instead of adding another source")
		}
		if errors.Is(err, mutation.ErrConflict) {
			return nil, hubapi.Conflict(0)
		}
		if err != nil {
			return nil, s.deps.RecordError(err)
		}
		if replay != nil {
			if replay.RequestHash != hash {
				return nil, hubapi.Conflict(0)
			}
			return content.Receipt(ctx, *replay)
		}
		return raw, nil
	}
}

func (s *Service) PrepareRunnerRecords(ctx context.Context, scope hubapi.Scope, request tracker.NativeRunEvent, facts RunnerFacts, now time.Time, historical ...billingstore.CostObservation) ([]billingstore.CostObservation, error) {
	if request.Type != "run.finished" || len(request.Data.Usage) == 0 {
		return nil, nil
	}
	observations, err := BuildRunnerObservations(request, s.deps.Prices, facts)
	if err != nil {
		return nil, err
	}
	records := make([]billingstore.CostObservation, 0, len(observations))
	for i := range observations {
		observation := &observations[i].Observation
		for _, record := range historical {
			old := record.Observation
			if record.OrganizationID == scope.Organization && record.ProjectID == scope.Project && old.Provider == observation.Provider && old.ProviderAccount == observation.ProviderAccount && old.SourceID == observation.SourceID && old.From.Equal(observation.From) && old.To.Equal(observation.To) && old.AttemptID == request.Data.AttemptID && old.WorkItemID == facts.WorkItemID && old.RunnerID == facts.RunnerID && old.MachineID == facts.MachineID {
				observation.Placement = old.Placement
				break
			}
		}
		if err := Normalize(observation, now); err != nil {
			return nil, err
		}
		for _, prior := range observations[:i] {
			previous := prior.Observation
			if observation.Provider == previous.Provider && observation.Metric == previous.Metric && observation.From.Before(previous.To) && previous.From.Before(observation.To) {
				return nil, hubapi.PublicInvalid("Overlapping usage must correct the original source period instead of adding another source")
			}
		}
	}
	if err := s.refuseKnownCostConflict(ctx, scope, observations, now); err != nil {
		return nil, err
	}
	for i := range observations {
		observation := &observations[i].Observation
		raw, err := json.Marshal(observation)
		if err != nil {
			return nil, err
		}
		ref, err := s.deps.Details.StoreDetail(ctx, string(scope.Organization), raw)
		if err != nil {
			return nil, err
		}
		if err := s.deps.ValidateDetail(ctx, s.deps.Details, scope.Organization, ref, raw); err != nil {
			return nil, err
		}
		records = append(records, billingstore.CostObservation{OrganizationID: scope.Organization, ProjectID: scope.Project, Observation: *observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now})
	}
	return records, nil
}

func (s *Service) refuseKnownCostConflict(ctx context.Context, scope hubapi.Scope, observations []RunnerObservation, now time.Time) error {
	prepared := make([]billingstore.CostObservation, 0, len(observations))
	for _, observation := range observations {
		raw, err := json.Marshal(observation.Observation)
		if err != nil {
			return err
		}
		ref, _, err := attachment.PrepareArchive(string(scope.Organization), 1, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		prepared = append(prepared, billingstore.CostObservation{OrganizationID: scope.Organization, ProjectID: scope.Project, Observation: observation.Observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now})
	}
	return s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(billingstore.CostConflictReader)
		if !ok {
			return nil
		}
		conflict, err := reader.BillingCostConflict(ctx, prepared)
		if err != nil {
			return err
		}
		if conflict {
			return mutation.ErrConflict
		}
		return nil
	})
}
