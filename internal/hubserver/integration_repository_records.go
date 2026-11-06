package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) bindCheckoutRecordCommand(ctx context.Context, scope nativeScope, options nativeCommandOptions, request bindNativeRepositoryRequest) (json.RawMessage, error) {
	owner, name, valid := splitRepositoryFullName(request.Repository)
	if !valid || !validCheckoutRepository(owner+"/"+name) {
		return nil, repositoryBindingRefusal("repository_invalid", "Repository must be owner/name")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	current, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if current.Settings.Revision != int64(request.ExpectedRevision) {
		return nil, nativeConflict(tracker.Revision(current.Settings.Revision))
	}
	if current.Core.Profile != "native" {
		return nil, repositoryBindingRefusal("repository_profile_invalid", "Only a native project can associate a runner checkout")
	}
	repository := owner + "/" + name
	if current.Settings.CheckoutRepository != "" && !strings.EqualFold(current.Settings.CheckoutRepository, repository) {
		return nil, repositoryBindingRefusal("repository_already_bound", "This project already has a checkout repository")
	}
	if current.Settings.Repository != "" && !strings.EqualFold(current.Settings.Repository, repository) {
		return nil, repositoryBindingRefusal("repository_already_bound", "This project already has a repository")
	}
	planned := current
	changed := current.Settings.CheckoutRepository == ""
	value := projectIntegrationRecord(current)
	if changed {
		planned.Settings.CheckoutRepository = repository
		planned.Settings.Intake = "manual"
		planned.States = nativeTriageStates(planned.States)
		value = projectIntegrationRecord(planned)
		value.Revision++
	}
	return s.tenantReceiptMutation(ctx, scope, options.OperationID, request.Mutation, request, value, func(w tenantstore.Writer) error {
		latest, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if latest.Settings.Revision != current.Settings.Revision {
			return nativeConflict(tracker.Revision(latest.Settings.Revision))
		}
		if !changed {
			return nil
		}
		if err := w.RequireProjectIdle(ctx, scope.project, s.config.now()); err != nil {
			if errors.Is(err, projectstore.ErrActiveWork) {
				return repositoryBindingRefusal("repository_binding_busy", "Finish active leases before changing integration authority")
			}
			return err
		}
		_, err = w.UpdateProjectRecord(ctx, planned)
		return err
	})
}
