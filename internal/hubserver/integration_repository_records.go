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

func (s *Service) bindGitHubRepositoryRecordCommand(ctx context.Context, scope nativeScope, options nativeCommandOptions, request bindNativeRepositoryRequest) (json.RawMessage, error) {
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
	if s.config.ReconcileBackend == nil {
		return nil, nativePublicInvalid("GitHub repository transport is not configured")
	}
	current, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if current.Settings.Revision != int64(request.ExpectedRevision) {
		return nil, nativeConflict(tracker.Revision(current.Settings.Revision))
	}
	owner, name, _ := splitRepositoryFullName(request.Repository)
	repository := strings.ToLower(owner + "/" + name)
	if current.Core.Profile != "native" || current.Settings.Repository != "" && !strings.EqualFold(current.Settings.Repository, repository) || current.Settings.CheckoutRepository != "" && !strings.EqualFold(current.Settings.CheckoutRepository, repository) {
		return nil, repositoryBindingRefusal("repository_already_bound", "Only an unbound native project can attach a repository")
	}
	snapshot, err := s.config.ReconcileBackend.Reconcile(ctx, ReconcileRequest{Repository: RepositoryTarget{Owner: owner, Name: name}, Profile: "native", SkipIssues: true, SkipRepository: true})
	if err != nil {
		return nil, err
	}
	if err := validateReconcileSnapshot(ReconcileSnapshot{Repository: snapshot.Repository}); err != nil {
		return nil, err
	}
	if !strings.EqualFold(snapshot.Repository.Owner+"/"+snapshot.Repository.Name, repository) {
		return nil, nativePublicInvalid("GitHub returned a different repository")
	}
	planned := current
	planned.Settings.Repository, planned.Settings.Intake = repository, "manual"
	planned.States = nativeTriageStates(planned.States)
	value := projectIntegrationRecord(planned)
	value.Revision++
	return s.tenantReceiptMutation(ctx, scope, options.OperationID, request.Mutation, request, value, func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "github_integration", s.config.now()); err != nil {
			return err
		}
		latest, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if latest.Settings.Revision != current.Settings.Revision {
			return nativeConflict(tracker.Revision(latest.Settings.Revision))
		}
		if err := w.RequireProjectIdle(ctx, scope.project, s.config.now()); err != nil {
			return err
		}
		_, err = w.UpdateProjectRecord(ctx, planned)
		if err != nil {
			return err
		}
		gitHubReconciliationWriter, ok := w.(projectstore.GitHubReconciliationWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return gitHubReconciliationWriter.SaveGitHubReconciliation(ctx, projectstore.GitHubReconciliation{ProjectID: scope.project, Repository: repository, NodeID: snapshot.Repository.NodeID, NextAttemptAt: s.config.now().Add(s.config.ReconcileInterval)})
	})
}
