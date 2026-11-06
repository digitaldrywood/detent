package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) accountRunnerUsage(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	var source leasestore.Attempt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(c.Param("item")))
		if err != nil {
			return err
		}
		source, err = r.Attempt(ctx, c.Param("attempt"))
		if err != nil {
			return err
		}
		if source.IssueID != item.ID {
			return nativeNotFound()
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	stored, err := s.readNativeAttemptTenant(ctx, scope, c.Param("item"), source.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if stored.Sequence != source.Sequence || stored.LeaseID != tracker.LeaseID(source.LeaseID) || stored.FencingToken != tracker.FencingToken(source.FencingToken) {
		return s.nativeAPIError(c, corestore.ErrRevisionConflict)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		owner, ok := w.(leasestore.RunnerUsageWriter)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		return owner.AccountRunnerTokenUsage(ctx, source, stored.Usage)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
