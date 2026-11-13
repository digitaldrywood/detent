package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func pullRequestRefreshLimited() error {
	return &nativeError{
		Code: "refresh_limited", Message: "Pull request refresh is limited to one per organization every ten seconds",
		status:        http.StatusTooManyRequests,
		Details:       map[string]any{"retry_after_seconds": int(pullRequestRefreshInterval / time.Second)},
		publicMessage: true,
	}
}

// Reads.

// projectRepositoryBinding reads the configured source repository independently
// of whether its GitHub tracker integration is enabled. Explicit native release
// delivery verifies this binding without enabling that integration.
func (s *Service) projectRepositoryBinding(ctx context.Context, query nativeQueryer, scope nativeScope) (*pullRequestConnector, int64, bool, error) {
	var project projectstore.Record
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(ctx, scope.project)
		return err
	}); err != nil {
		return nil, 0, false, err
	}
	if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !validCheckoutRepository(project.Settings.CheckoutRepository) {
		return nil, 0, false, nil
	}
	verified, err := s.verifiedProjectCheckout(ctx, query, scope, project.Settings.CheckoutRepository, s.config.now().UTC())
	if err != nil || !verified {
		return nil, 0, false, err
	}
	return &pullRequestConnector{Provider: "github", Repository: project.Settings.CheckoutRepository}, 0, project.Settings.RepositoryEnabled, nil
}

func (s *Service) changePullRequestView(ctx context.Context, scope nativeScope, item string, change tracker.ChangeRequest,
	connector *pullRequestConnector, repositoryID int64, now time.Time) (pullRequestView, error) {
	view, _, err := s.changePullRequestViewSource(ctx, scope, item, change, connector, repositoryID, now)
	return view, err
}

func (s *Service) changePullRequestViewSource(ctx context.Context, scope nativeScope, item string, change tracker.ChangeRequest,
	connector *pullRequestConnector, repositoryID int64, now time.Time) (pullRequestView, projectstore.GitHubPullRequestRefresh, error) {
	target := projectstore.GitHubPullRequestRefresh{GitHubPullRequestRefreshKey: projectstore.GitHubPullRequestRefreshKey{ProjectID: scope.project, ChangeID: change.ID}, WorkItemID: tracker.NativeWorkItemID(item), RequestedAt: now}
	if connector != nil {
		target.Repository = strings.ToLower(connector.Repository)
	}
	detail, err := s.selectedChangeDetail(ctx, scope, item, change.ID, now)
	if err != nil {
		return pullRequestView{}, target, err
	}
	change = detail.Change
	view := pullRequestView{
		ID: change.ID, ChangeID: change.ID, Title: change.Title, State: "open",
		Head: pullRequestRef{}, Base: pullRequestRef{}, Checks: []pullRequestCheck{}, Reviews: []pullRequestReview{},
		Labels: []string{}, UpdatedAt: change.UpdatedAt, FetchedAt: change.UpdatedAt,
	}
	delivery, associated, err := s.readReleaseDeliveryRecord(ctx, scope, item)
	if err != nil {
		return view, target, err
	}
	if associated && delivery.ChangeID == change.ID {
		view.Number = delivery.PRNumber
		view.URL = fmt.Sprintf("%s/pull/%d", delivery.Repository, delivery.PRNumber)
		view.Head = pullRequestRef{Ref: delivery.HeadRef, SHA: delivery.HeadSHA, Repository: delivery.Repository}
		view.Base = pullRequestRef{Ref: delivery.BaseRef, SHA: delivery.BaseSHA, Repository: delivery.Repository}
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
			view.Number = externalPullRequestNumber(*current.External)
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
		view.Checks = append(view.Checks, pullRequestCheck{Name: name, Status: status, Conclusion: check.Conclusion, URL: url, CompletedAt: completed})
	}
	for _, review := range detail.Reviews {
		view.Reviews = append(view.Reviews, pullRequestReview{Author: review.Actor.PrincipalID, State: review.Decision, SubmittedAt: review.CreatedAt})
	}
	view.ReviewDecision = detail.Summary.NativeReview
	target.Number = view.Number
	if connector != nil && current != nil && view.Number > 0 && current.HeadSHA != "" {
		var observation *projectstore.GitHubPullRequestObservation
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeBulkExportItem(ctx, r, scope, tracker.NativeWorkItemID(item)); err != nil {
				return err
			}
			project, err := r.ProjectRecord(ctx, scope.project)
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
			if live.ProjectID != scope.project || live.CurrentVersionID == nil || *live.CurrentVersionID != current.ID {
				return corestore.ErrRevisionConflict
			}
			reader, ok := r.(projectstore.GitHubPullRequestRefreshReader)
			if !ok {
				return ErrPostgresFeatureUnavailable
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
			view.Mergeable = pullRequestMergeable{}
			if observation.Mergeable != nil {
				view.Mergeable = pullRequestMergeable{Known: true, Value: *observation.Mergeable}
			}
			view.FetchedAt = observation.ObservedAt
			view.Connector = &pullRequestConnector{Provider: "github", Repository: target.Repository, SynchronizedAt: observation.ObservedAt}
		}
	}
	return view, target, nil
}

// listWorkItemPullRequests answers the panel. It follows the issue's read
// rule: the item is resolved in the caller's scope first, so a reader who
// cannot see the issue cannot see its pull requests.
func (s *Service) listWorkItemPullRequests(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams(), "refresh"); err != nil {
		return s.nativeAPIError(c, err)
	}
	refresh := strings.TrimSpace(c.QueryParam("refresh"))
	if refresh != "" && refresh != "1" {
		return s.nativeAPIError(c, nativePublicInvalid("refresh accepts only 1"))
	}
	views, err := s.readWorkItemPullRequests(c.Request().Context(), nativeRequestScope(c), c.Param("item"), refresh)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return s.nativeAPIError(c, nativeConflict(0))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, views)
}

func (s *Service) readWorkItemPullRequests(ctx context.Context, scope nativeScope, item, refresh string) ([]pullRequestView, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if _, _, err := s.readWorkItemRecord(readCtx, scope, item); err != nil {
		return nil, err
	}
	now := s.config.now()
	key := pullRequestCacheKey(scope, item)
	cacheable := scope.credential.HostedRole != "reporter"
	if refresh == "" {
		if cached, ok := s.pullRequests.get(key, now); cacheable && ok {
			if err := s.authorizeLinkedChangeView(readCtx, scope, item); err != nil {
				return nil, err
			}
			current, err := s.cachedPullRequestSourcesCurrent(readCtx, scope, cached)
			if err != nil {
				return nil, err
			}
			if current {
				return cached, nil
			}
			s.pullRequests.invalidate(key)
		}
	} else if !s.pullRequests.allowRefresh(scope.organization, now) {
		return nil, pullRequestRefreshLimited()
	}
	connector, repositoryID, enabled, err := s.projectRepositoryBinding(readCtx, nil, scope)
	if err != nil {
		return nil, err
	}
	if !enabled {
		connector = nil
	}
	changes, err := s.listChangeRecords(readCtx, scope, item)
	if err != nil {
		return nil, err
	}
	views := []pullRequestView{}
	requests := make([]projectstore.GitHubPullRequestRefresh, 0, len(changes))
	for _, change := range changes {
		view, target, err := s.changePullRequestViewSource(readCtx, scope, item, change, connector, repositoryID, now)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
		if view.Number > 0 {
			requests = append(requests, target)
		}
	}
	if refresh == "1" && connector != nil {
		if err := s.requestPullRequestHydration(ctx, scope, item, connector.Repository, requests); err != nil {
			return nil, err
		}
	}
	if err := s.authorizeLinkedChangeView(readCtx, scope, item); err != nil {
		return nil, err
	}
	if cacheable {
		s.pullRequests.set(key, views, now)
	}
	return views, nil
}

func (s *Service) cachedPullRequestSourcesCurrent(ctx context.Context, scope nativeScope, views []pullRequestView) (bool, error) {
	connected := false
	for _, view := range views {
		connected = connected || view.Connector != nil
	}
	if !connected {
		return true, nil
	}
	connector, _, enabled, err := s.projectRepositoryBinding(ctx, nil, scope)
	if err != nil {
		return false, err
	}
	if !enabled || connector == nil {
		return false, nil
	}
	current := true
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
			if change.ProjectID != scope.project || change.CurrentVersionID == nil {
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

func (s *Service) authorizeLinkedChangeView(ctx context.Context, scope nativeScope, item string) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeBulkExportItem(ctx, r, scope, tracker.NativeWorkItemID(item))
	})
}

func (s *Service) requestPullRequestHydration(ctx context.Context, scope nativeScope, item, repository string, requests []projectstore.GitHubPullRequestRefresh) error {
	if len(requests) == 0 {
		return nil
	}
	if len(requests) > corestore.MaxPageSize {
		return corestore.ErrInvalidPage
	}
	if s.config.TenantRepository == nil || s.config.ReconcileBackend == nil {
		return ErrPostgresFeatureUnavailable
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeBulkExportItem(ctx, w, scope, tracker.NativeWorkItemID(item)); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !project.Settings.RepositoryEnabled || !strings.EqualFold(project.Settings.CheckoutRepository, repository) {
			return corestore.ErrRevisionConflict
		}
		for _, request := range requests {
			if request.ProjectID != scope.project || request.WorkItemID != tracker.NativeWorkItemID(item) || request.Repository != strings.ToLower(repository) {
				return corestore.ErrRevisionConflict
			}
			current, err := w.Change(ctx, request.ChangeID)
			if err != nil {
				return err
			}
			if current.ProjectID != scope.project || current.CurrentVersionID == nil || *current.CurrentVersionID != request.RequestedVersion {
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
			return ErrPostgresFeatureUnavailable
		}
		return writer.RequestGitHubPullRequests(ctx, requests)
	})
}
