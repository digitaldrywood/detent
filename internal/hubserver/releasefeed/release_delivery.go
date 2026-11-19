package releasefeed

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Delivery) GetReleaseDeliveryStatus(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
	issue, err := s.readReleaseDeliveryIssue(ctx, scope, c.Param("item"))
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if issue.ReleaseDelivery == nil {
		return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
	}
	verifier, ok := s.deps.DeliveryVerifier()
	if !ok {
		return s.deps.NativeAPIError(c, errors.New("release repository transport is unavailable"))
	}
	status, err := verifier.ReleaseDeliveryStatus(ctx, issue.ReleaseDelivery.ReleasePickInput)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	current, err := s.readReleaseDeliveryIssue(ctx, scope, c.Param("item"))
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if current.Revision != issue.Revision || current.ReleaseDelivery == nil || *current.ReleaseDelivery != *issue.ReleaseDelivery {
		return s.deps.NativeAPIError(c, hubapi.NativeConflict(current.Revision))
	}
	return c.JSON(http.StatusOK, status)
}

func (s *Delivery) prepareReleasePick(ctx context.Context, scope hubscope.NativeScope, item string, r tracker.AssociateReleasePick) error {
	if scope.Credential.Scope != hubapi.APIScopeOperator && scope.Credential.Scope != hubapi.APIScopeAdmin || scope.Credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if err := s.deps.ValidateReleasePick(r); err != nil {
		return err
	}
	if s.deps.Repository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	repository, configured, err := s.deps.CheckoutRepository(ctx, scope)
	if err != nil {
		return err
	}
	if !configured || r.Repository != "https://github.com/"+repository {
		return hubapi.NativePublicInvalid("Release PR repository must match the project's configured repository")
	}
	existing, found, err := s.ReadReleaseDeliveryRecord(ctx, scope, item)
	if err != nil {
		return err
	}
	if found && existing.ReleasePickInput == r.ReleasePickInput {
		return nil
	}
	verifier, ok := s.deps.PickVerifier()
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
		if errors.Is(err, githubcontract.ErrReleasePickMismatch) {
			return hubapi.NativePublicInvalid("Release PR provenance verification failed")
		}
		return fmt.Errorf("verify release PR: %w", err)
	}
	return nil
}

func (s *Delivery) AssociateReleasePick(c echo.Context) error {
	var r tracker.AssociateReleasePick
	if err := hubapi.DecodeAPIJSON(c, &r); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	if err := s.prepareReleasePick(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), r); err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	ctx := hubcontent.IsolateContentContext(s.deps.RunDetailContext(c.Request().Context()))
	raw, err := s.AssociateReleasePickTenant(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Request().Method+" "+c.Request().URL.EscapedPath(), r)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	s.deps.InvalidatePullRequests(hubscope.NativeRequestScope(c), c.Param("item"))
	return c.JSONBlob(http.StatusOK, raw)
}
