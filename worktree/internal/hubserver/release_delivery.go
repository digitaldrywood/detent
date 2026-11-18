package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) getReleaseDeliveryStatus(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
	issue, err := s.readReleaseDeliveryIssue(ctx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if issue.ReleaseDelivery == nil {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	verifier, ok := s.config.ReconcileBackend.(ReleaseDeliveryVerifier)
	if !ok {
		return s.nativeAPIError(c, errors.New("release repository transport is unavailable"))
	}
	status, err := verifier.ReleaseDeliveryStatus(ctx, issue.ReleaseDelivery.ReleasePickInput)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	current, err := s.readReleaseDeliveryIssue(ctx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if current.Revision != issue.Revision || current.ReleaseDelivery == nil || *current.ReleaseDelivery != *issue.ReleaseDelivery {
		return s.nativeAPIError(c, hubapi.NativeConflict(current.Revision))
	}
	return c.JSON(http.StatusOK, status)
}

func (s *Service) prepareReleasePick(ctx context.Context, scope hubscope.NativeScope, item string, r tracker.AssociateReleasePick) error {
	if scope.Credential.Scope != hubapi.APIScopeOperator && scope.Credential.Scope != hubapi.APIScopeAdmin || scope.Credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if err := validateReleasePick(r); err != nil {
		return err
	}
	if s.config.TenantRepository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	connector, _, _, err := s.projectRepositoryBinding(ctx, nil, scope)
	if err != nil {
		return err
	}
	if connector == nil || r.Repository != "https://github.com/"+connector.Repository {
		return hubapi.NativePublicInvalid("Release PR repository must match the project's configured repository")
	}
	existing, found, err := s.readReleaseDeliveryRecord(ctx, scope, item)
	if err != nil {
		return err
	}
	if found && existing.ReleasePickInput == r.ReleasePickInput {
		return nil
	}
	verifier, ok := s.config.ReconcileBackend.(ReleasePickVerifier)
	if !ok {
		return errors.New("repository transport does not support release provenance verification")
	}
	verify := func() error { return verifier.VerifyReleasePick(ctx, r.ReleasePickInput) }
	if found {
		verify = func() error {
			return verifier.VerifyReleasePickRefresh(ctx, existing.ReleasePickInput, r.ReleasePickInput)
		}
	}
	if err := verify(); err != nil {
		if errors.Is(err, ErrReleasePickMismatch) {
			return hubapi.NativePublicInvalid("Release PR provenance verification failed")
		}
		return fmt.Errorf("verify release PR: %w", err)
	}
	return nil
}

func (s *Service) associateReleasePick(c echo.Context) error {
	var r tracker.AssociateReleasePick
	if err := hubapi.DecodeAPIJSON(c, &r); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.prepareReleasePick(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), r); err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx := hubcontent.IsolateContentContext(s.runDetailContext(c.Request().Context()))
	raw, err := s.associateReleasePickTenant(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Request().Method+" "+c.Request().URL.EscapedPath(), r)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.pullRequests.invalidate(pullRequestCacheKey(hubscope.NativeRequestScope(c), c.Param("item")))
	return c.JSONBlob(http.StatusOK, raw)
}
