package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository         tenantstore.Repository
	Now                func() time.Time
	Cache              *Cache
	TransportAvailable bool
	Unavailable        error
	ValidCheckout      func(string) bool
	VerifiedCheckout   func(context.Context, string, time.Time) (bool, error)
	ReadItem           func(context.Context, string) error
	ChangeDetail       func(context.Context, string, string, time.Time) (tracker.ChangeDetail, error)
	ReleaseDelivery    func(context.Context, string) (tracker.ReleaseDelivery, bool, error)
	AuthorizeItem      func(context.Context, tenantstore.Reader, tracker.NativeWorkItemID) error
	Changes            func(context.Context, string) ([]tracker.ChangeRequest, error)
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func RefreshLimited() error {
	return &hubapi.Error{
		Code: "refresh_limited", Message: "Pull request refresh is limited to one per organization every ten seconds",
		Status:  http.StatusTooManyRequests,
		Details: map[string]any{"retry_after_seconds": int(RefreshInterval / time.Second)},
		Public:  true,
	}
}

func (s *Service) RepositoryBinding(ctx context.Context, scope hubapi.Scope) (*Connector, int64, bool, error) {
	var project projectstore.Metadata
	if err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectMetadata(ctx, scope.Project)
		return err
	}); err != nil {
		return nil, 0, false, err
	}
	if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !s.deps.ValidCheckout(project.Settings.CheckoutRepository) {
		return nil, 0, false, nil
	}
	verified, err := s.deps.VerifiedCheckout(ctx, project.Settings.CheckoutRepository, s.deps.Now().UTC())
	if err != nil || !verified {
		return nil, 0, false, err
	}
	return &Connector{Provider: "github", Repository: project.Settings.CheckoutRepository}, 0, project.Settings.RepositoryEnabled, nil
}

func (s *Service) ChangeViewSource(ctx context.Context, scope hubapi.Scope, item string, change tracker.ChangeRequest,
	connector *Connector, repositoryID int64, now time.Time) (View, projectstore.GitHubPullRequestRefresh, error) {
	target := projectstore.GitHubPullRequestRefresh{GitHubPullRequestRefreshKey: projectstore.GitHubPullRequestRefreshKey{ProjectID: scope.Project, ChangeID: change.ID}, WorkItemID: tracker.NativeWorkItemID(item), RequestedAt: now}
	if connector != nil {
		target.Repository = strings.ToLower(connector.Repository)
	}
	detail, err := s.deps.ChangeDetail(ctx, item, change.ID, now)
	if err != nil {
		return View{}, target, err
	}
	change = detail.Change
	view := View{
		ID: change.ID, ChangeID: change.ID, Title: change.Title, State: "open",
		Head: Ref{}, Base: Ref{}, Checks: []Check{}, Reviews: []Review{},
		Labels: []string{}, UpdatedAt: change.UpdatedAt, FetchedAt: change.UpdatedAt,
	}
	delivery, associated, err := s.deps.ReleaseDelivery(ctx, item)
	if err != nil {
		return view, target, err
	}
	if associated && delivery.ChangeID == change.ID {
		view.Number = delivery.PRNumber
		view.URL = fmt.Sprintf("%s/pull/%d", delivery.Repository, delivery.PRNumber)
		view.Head = Ref{Ref: delivery.HeadRef, SHA: delivery.HeadSHA, Repository: delivery.Repository}
		view.Base = Ref{Ref: delivery.BaseRef, SHA: delivery.BaseSHA, Repository: delivery.Repository}
	}
	var current *tracker.ChangeVersion
	for index := range detail.Versions {
		if detail.Versions[index].ID == change.CurrentVersion {
			current = &detail.Versions[index]
		}
	}
	if current != nil {
		target.RequestedVersion = current.ID
		target.RequestedHeadSHA = current.HeadSHA
		view.Head.SHA, view.Base.SHA = current.HeadSHA, current.BaseSHA
		view.Head.Repository = current.Repository
		view.Author = current.Actor.PrincipalID
		if current.External != nil {
			view.URL = current.External.URL
			view.Number = ExternalNumber(*current.External)
		}
	}
	for _, check := range detail.Checks {
		status := "in_progress"
		var completed *time.Time
		if !check.CompletedAt.IsZero() {
			status = "completed"
			at := check.CompletedAt
			completed = &at
		}
		name := check.WorkflowID
		if strings.TrimSpace(name) == "" {
			name = check.CheckRunID
		}
		url := ""
		if len(check.Evidence) > 0 {
			url = check.Evidence[0].URI
		}
		view.Checks = append(view.Checks, Check{Name: name, Status: status, Conclusion: check.Conclusion, URL: url, CompletedAt: completed})
	}
	for _, review := range detail.Reviews {
		view.Reviews = append(view.Reviews, Review{Author: review.Actor.PrincipalID, State: review.Decision, SubmittedAt: review.CreatedAt})
	}
	view.ReviewDecision = detail.Summary.NativeReview
	target.Number = view.Number
	if connector != nil && current != nil && view.Number > 0 && current.HeadSHA != "" {
		var observation *projectstore.GitHubPullRequestObservation
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.deps.AuthorizeItem(ctx, r, tracker.NativeWorkItemID(item)); err != nil {
				return err
			}
			project, err := r.ProjectMetadata(ctx, scope.Project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil || !project.Settings.RepositoryEnabled || !strings.EqualFold(project.Settings.CheckoutRepository, connector.Repository) {
				return corestore.ErrRevisionConflict
			}
			live, err := r.Change(ctx, change.ID)
			if err != nil {
				return err
			}
			if live.ProjectID != scope.Project || live.CurrentVersionID == nil || *live.CurrentVersionID != current.ID {
				return corestore.ErrRevisionConflict
			}
			reader, ok := r.(projectstore.GitHubPullRequestRefreshReader)
			if !ok {
				return s.deps.Unavailable
			}
			row, err := reader.GitHubPullRequestRefresh(ctx, target.GitHubPullRequestRefreshKey)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if row.Observation != nil && row.Observation.Repository == target.Repository && row.Observation.HeadSHA == current.HeadSHA {
				observation = row.Observation
			}
			return nil
		})
		if err != nil {
			return view, target, err
		}
		if observation != nil {
			view.State = observation.State
			view.Draft = observation.Draft
			view.Mergeable = Mergeable{}
			if observation.Mergeable != nil {
				view.Mergeable = Mergeable{Known: true, Value: *observation.Mergeable}
			}
			view.FetchedAt = observation.ObservedAt
			view.Connector = &Connector{Provider: "github", Repository: target.Repository, SynchronizedAt: observation.ObservedAt}
		}
	}
	return view, target, nil
}

func (s *Service) Read(ctx context.Context, scope hubapi.Scope, item, refresh string) ([]View, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	if err := s.deps.ReadItem(readCtx, item); err != nil {
		return nil, err
	}
	now := s.deps.Now()
	key := CacheKey(scope.Organization, scope.Project, item)
	cacheable := scope.HostedRole != "reporter"
	if refresh == "" {
		if cached, ok := s.deps.Cache.Get(key, now); cacheable && ok {
			if err := s.authorizeView(readCtx, scope, item); err != nil {
				return nil, err
			}
			current, err := s.sourcesCurrent(readCtx, scope, cached)
			if err != nil {
				return nil, err
			}
			if current {
				return cached, nil
			}
			s.deps.Cache.Invalidate(key)
		}
	} else if !s.deps.Cache.AllowRefresh(scope.Organization, now) {
		return nil, RefreshLimited()
	}
	connector, repositoryID, enabled, err := s.RepositoryBinding(readCtx, scope)
	if err != nil {
		return nil, err
	}
	if !enabled {
		connector = nil
	}
	changes, err := s.deps.Changes(readCtx, item)
	if err != nil {
		return nil, err
	}
	views := []View{}
	requests := make([]projectstore.GitHubPullRequestRefresh, 0, len(changes))
	for _, change := range changes {
		view, target, err := s.ChangeViewSource(readCtx, scope, item, change, connector, repositoryID, now)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
		if view.Number > 0 {
			requests = append(requests, target)
		}
	}
	if refresh == "1" && connector != nil {
		if err := s.requestHydration(ctx, scope, item, connector.Repository, requests); err != nil {
			return nil, err
		}
	}
	if err := s.authorizeView(readCtx, scope, item); err != nil {
		return nil, err
	}
	if cacheable {
		s.deps.Cache.Set(key, views, now)
	}
	return views, nil
}

func (s *Service) sourcesCurrent(ctx context.Context, scope hubapi.Scope, views []View) (bool, error) {
	connected := false
	for _, view := range views {
		connected = connected || view.Connector != nil
	}
	if !connected {
		return true, nil
	}
	connector, _, enabled, err := s.RepositoryBinding(ctx, scope)
	if err != nil {
		return false, err
	}
	if !enabled || connector == nil {
		return false, nil
	}
	current := true
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		for _, view := range views {
			if view.Connector == nil {
				continue
			}
			if !strings.EqualFold(view.Connector.Repository, connector.Repository) {
				current = false
				return nil
			}
			change, err := r.Change(ctx, view.ChangeID)
			if errors.Is(err, corestore.ErrNotFound) {
				current = false
				return nil
			}
			if err != nil {
				return err
			}
			if change.ProjectID != scope.Project || change.CurrentVersionID == nil {
				current = false
				return nil
			}
			version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			if version.HeadSHA != view.Head.SHA {
				current = false
				return nil
			}
		}
		return nil
	})
	return current, err
}

func (s *Service) authorizeView(ctx context.Context, scope hubapi.Scope, item string) error {
	return s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.deps.AuthorizeItem(ctx, r, tracker.NativeWorkItemID(item))
	})
}

func (s *Service) requestHydration(ctx context.Context, scope hubapi.Scope, item, repository string, requests []projectstore.GitHubPullRequestRefresh) error {
	if len(requests) == 0 {
		return nil
	}
	if len(requests) > corestore.MaxPageSize {
		return corestore.ErrInvalidPage
	}
	if s.deps.Repository == nil || !s.deps.TransportAvailable {
		return s.deps.Unavailable
	}
	return s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.deps.AuthorizeItem(ctx, w, tracker.NativeWorkItemID(item)); err != nil {
			return err
		}
		project, err := w.ProjectMetadata(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !project.Settings.RepositoryEnabled || !strings.EqualFold(project.Settings.CheckoutRepository, repository) {
			return corestore.ErrRevisionConflict
		}
		for _, request := range requests {
			if request.ProjectID != scope.Project || request.WorkItemID != tracker.NativeWorkItemID(item) || request.Repository != strings.ToLower(repository) {
				return corestore.ErrRevisionConflict
			}
			current, err := w.Change(ctx, request.ChangeID)
			if err != nil {
				return err
			}
			if current.ProjectID != scope.Project || current.CurrentVersionID == nil || *current.CurrentVersionID != request.RequestedVersion {
				return corestore.ErrRevisionConflict
			}
			version, err := w.Version(ctx, current.ID, request.RequestedVersion)
			if err != nil {
				return err
			}
			if version.HeadSHA != request.RequestedHeadSHA {
				return corestore.ErrRevisionConflict
			}
		}
		writer, ok := w.(projectstore.GitHubPullRequestRefreshWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return writer.RequestGitHubPullRequests(ctx, requests)
	})
}
