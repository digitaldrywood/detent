package orgsettings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ModelRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision       `json:"expected_revision,string"`
	Selection        *config.ModelSelection `json:"selection"`
}

func (s *Service) ReadModels(ctx context.Context, organization tracker.OrganizationID) (projectsettings.ModelSelection, error) {
	var row policystore.OrganizationModels
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.OrganizationModels(ctx)
		return err
	})
	if err != nil {
		return projectsettings.ModelSelection{}, s.deps.RecordError(err)
	}
	return s.ModelsFromRecord(ctx, organization, row)
}

func (s *Service) ModelsFromRecord(ctx context.Context, organization tracker.OrganizationID, row policystore.OrganizationModels) (projectsettings.ModelSelection, error) {
	selection := ModelDefaults()
	if row.Revision > 0 {
		raw, err := eventstore.NewContent(s.deps.Details).Receipt(ctx, eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: row.Payload.Key, PayloadSHA256: row.Payload.SHA256, PayloadBytes: row.Payload.CompressedSize, PayloadContentBytes: row.Payload.UncompressedSize})
		if err != nil {
			return projectsettings.ModelSelection{}, err
		}
		selection = config.ModelSelection{}
		if err := json.Unmarshal(raw, &selection); err != nil {
			return projectsettings.ModelSelection{}, err
		}
	}
	effective := config.ResolveModelSelection(ModelDefaults(), selection)
	return projectsettings.ModelSelection{Revision: tracker.Revision(row.Revision), Selection: &selection, Effective: config.ResolveCloudModelSelection(effective, config.ModelSelection{})}, nil
}

func ModelDefaults() config.ModelSelection {
	selection := config.SolFirstModelSelection()
	selection.Preset = nil
	selection.NormalModel = new("gpt-6.1-sol")
	selection.Levels = map[string]config.ModelSelectionDefaults{
		"normal":       {Model: new("normal"), Effort: new("high")},
		"complex":      {Model: new("normal"), Effort: new("high")},
		"very_complex": {Model: new("complex"), Effort: new("medium")},
	}
	selection.Stages["plan"] = config.ModelSelectionStage{IssueComplexity: new(false), Model: new("complex"), Effort: new("low")}
	selection.Stages["validator"] = config.ModelSelectionStage{IssueComplexity: new(false), Model: new("complex"), Effort: new("medium")}
	return selection
}

func (s *Service) UpdateModels(ctx context.Context, scope hubapi.Scope, operation string, request ModelRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || request.ExpectedRevision < 0 || request.Selection == nil {
		return nil, hubapi.PublicInvalid("An idempotency key, nonnegative revision and model selection are required")
	}
	if s.deps.AccessRepository == nil {
		return nil, hubapi.NotFound()
	}
	if err := s.deps.Recheck(ctx); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if replay, found, err := s.deps.Receipt(ctx, operation, request.IdempotencyKey, hash); found || err != nil {
		return replay, err
	}
	effective := config.ResolveCloudModelSelection(*request.Selection, config.ModelSelection{})
	if len(effective.Validate()) > 0 {
		return nil, hubapi.PublicInvalid("Model selection is invalid")
	}
	effective = config.ResolveCloudModelSelection(config.ResolveModelSelection(ModelDefaults(), *request.Selection), config.ModelSelection{})
	if len(effective.Validate()) > 0 {
		return nil, hubapi.PublicInvalid("Model selection is invalid")
	}
	value := projectsettings.ModelSelection{Revision: request.ExpectedRevision + 1, Selection: request.Selection, Effective: effective}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	selection, err := json.Marshal(request.Selection)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.deps.Details)
	payload, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, selection)
	if err != nil {
		return nil, err
	}
	row := policystore.OrganizationModels{OrganizationID: scope.Organization, Revision: int64(value.Revision), Payload: eventstore.Reference(payload.PayloadObjectKey, payload.PayloadSHA256, payload.PayloadBytes, payload.PayloadContentBytes)}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.deps.Now()}, raw)
	if err != nil {
		return nil, err
	}
	var existing *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.Conflict(0)
			}
			existing = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.SetOrganizationModels(ctx, row, int64(request.ExpectedRevision)); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
		if authErr := s.deps.Recheck(ctx); authErr != nil {
			return nil, authErr
		}
		if replay, found, readErr := s.deps.Receipt(ctx, operation, request.IdempotencyKey, hash); found || readErr != nil {
			return replay, readErr
		}
		current, readErr := s.ReadModels(ctx, scope.Organization)
		if readErr != nil {
			return nil, readErr
		}
		return nil, hubapi.Conflict(current.Revision)
	}
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}
