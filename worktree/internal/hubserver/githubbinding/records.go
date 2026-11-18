package githubbinding

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/githubwebhook"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integrationpage"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"

	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) bindCheckout(ctx context.Context, scope hubapi.Scope, operation string, request Request) (json.RawMessage, error) {
	owner, name, valid := githubwebhook.SplitRepositoryFullName(request.Repository)
	if !valid || !runnerstore.ValidCheckoutRepository(owner+"/"+name) {
		return nil, Refusal("repository_invalid", "Repository must be owner/name")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.PublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.deps.Recheck(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.deps.Receipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	current, err := s.deps.ReadProject(ctx, scope)
	if err != nil {
		return nil, err
	}
	if current.Settings.Revision != int64(request.ExpectedRevision) {
		return nil, hubapi.Conflict(tracker.Revision(current.Settings.Revision))
	}
	if current.Core.Profile != "native" {
		return nil, Refusal("repository_profile_invalid", "Only a native project can associate a runner checkout")
	}
	repository := owner + "/" + name
	if current.Settings.CheckoutRepository != "" && !strings.EqualFold(current.Settings.CheckoutRepository, repository) {
		return nil, Refusal("repository_already_bound", "This project already has a checkout repository")
	}
	if current.Settings.Repository != "" && !strings.EqualFold(current.Settings.Repository, repository) {
		return nil, Refusal("repository_already_bound", "This project already has a repository")
	}
	planned := current
	changed := current.Settings.CheckoutRepository == ""
	value := integrationpage.Record(current)
	if changed {
		planned.Settings.CheckoutRepository = repository
		planned.Settings.Intake = "manual"
		planned.States = projectstore.TriageStates(planned.States)
		value = integrationpage.Record(planned)
		value.Revision++
	}
	return s.deps.Mutate(ctx, scope, operation, request.Mutation, request, value, func(w tenantstore.Writer) error {
		latest, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if latest.Settings.Revision != current.Settings.Revision {
			return hubapi.Conflict(tracker.Revision(latest.Settings.Revision))
		}
		if !changed {
			return nil
		}
		if err := w.RequireProjectIdle(ctx, scope.Project, s.deps.Now()); err != nil {
			if errors.Is(err, projectstore.ErrActiveWork) {
				return Refusal("repository_binding_busy", "Finish active leases before changing integration authority")
			}
			return err
		}
		_, err = w.UpdateProjectRecord(ctx, planned)
		return err
	})
}

func (s *Service) bindGitHub(ctx context.Context, scope hubapi.Scope, operation string, request Request) (json.RawMessage, error) {
	if err := s.deps.Recheck(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.deps.Receipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	if s.deps.Backend == nil {
		return nil, hubapi.PublicInvalid("GitHub repository transport is not configured")
	}
	current, err := s.deps.ReadProject(ctx, scope)
	if err != nil {
		return nil, err
	}
	if current.Settings.Revision != int64(request.ExpectedRevision) {
		return nil, hubapi.Conflict(tracker.Revision(current.Settings.Revision))
	}
	owner, name, _ := githubwebhook.SplitRepositoryFullName(request.Repository)
	repository := strings.ToLower(owner + "/" + name)
	if current.Core.Profile != "native" || current.Settings.Repository != "" && !strings.EqualFold(current.Settings.Repository, repository) || current.Settings.CheckoutRepository != "" && !strings.EqualFold(current.Settings.CheckoutRepository, repository) {
		return nil, Refusal("repository_already_bound", "Only an unbound native project can attach a repository")
	}
	snapshot, err := s.deps.Backend.Reconcile(ctx, githubcontract.ReconcileRequest{Repository: githubcontract.RepositoryTarget{Owner: owner, Name: name}, Profile: "native", SkipIssues: true, SkipRepository: true})
	if err != nil {
		return nil, err
	}
	if err := ValidateSnapshot(githubcontract.ReconcileSnapshot{Repository: snapshot.Repository}); err != nil {
		return nil, err
	}
	if !strings.EqualFold(snapshot.Repository.Owner+"/"+snapshot.Repository.Name, repository) {
		return nil, hubapi.PublicInvalid("GitHub returned a different repository")
	}
	planned := current
	planned.Settings.Repository, planned.Settings.Intake = repository, "manual"
	planned.States = projectstore.TriageStates(planned.States)
	value := integrationpage.Record(planned)
	value.Revision++
	return s.deps.Mutate(ctx, scope, operation, request.Mutation, request, value, func(w tenantstore.Writer) error {
		if err := s.deps.RequireFeature(ctx, w, "github_integration", s.deps.Now()); err != nil {
			return err
		}
		latest, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if latest.Settings.Revision != current.Settings.Revision {
			return hubapi.Conflict(tracker.Revision(latest.Settings.Revision))
		}
		if err := w.RequireProjectIdle(ctx, scope.Project, s.deps.Now()); err != nil {
			return err
		}
		_, err = w.UpdateProjectRecord(ctx, planned)
		return err
	})
}
