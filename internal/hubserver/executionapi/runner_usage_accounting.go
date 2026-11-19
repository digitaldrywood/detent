package executionapi

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type UsageDeps struct {
	Repository       tenantstore.Repository
	HTTPError        func(echo.Context, error, ...any) error
	AuthorizeAccess  func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	ReadAttempt      func(context.Context, hubscope.NativeScope, string, string) (tracker.NativeAttempt, error)
	Plans            func() *hostedplans.HostedPlansConfig
	Entitlement      func(context.Context, billingstore.Reader, time.Time) (hostedplans.HostedEntitlement, error)
	HasRequestBudget func(context.Context) bool
}

func (d UsageDeps) AccountRunnerUsage(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	var source leasestore.Attempt
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, true); err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(c.Param("item")))
		if err != nil {
			return err
		}
		source, err = r.Attempt(ctx, c.Param("attempt"))
		if err != nil {
			return err
		}
		if source.IssueID != item.ID {
			return hubapi.NativeNotFound()
		}
		return nil
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	stored, err := d.ReadAttempt(ctx, scope, c.Param("item"), source.ID)
	if err != nil {
		return d.HTTPError(c, err)
	}
	if stored.Sequence != source.Sequence || stored.LeaseID != tracker.LeaseID(source.LeaseID) || stored.FencingToken != tracker.FencingToken(source.FencingToken) {
		return d.HTTPError(c, corestore.ErrRevisionConflict)
	}
	err = d.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := d.AuthorizeAccess(ctx, w, scope, true); err != nil {
			return err
		}
		owner, ok := w.(leasestore.RunnerUsageWriter)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		return owner.AccountRunnerTokenUsage(ctx, source, stored.Usage)
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
