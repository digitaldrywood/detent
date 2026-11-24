package claimapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WaitDeps struct {
	NewTimer        func(time.Duration) *time.Timer
	Repository      tenantstore.Repository
	Now             func() time.Time
	HTTPError       func(echo.Context, error, ...any) error
	AuthorizeAccess func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	Cursor          func(string) string
	NotificationKey func(tracker.OrganizationID) string
	Subscribe       func(string) (*eventbroker.Subscription, func())
}

func (d WaitDeps) WaitNativeCandidates(c echo.Context) error {
	wait := 30
	if value := c.QueryParam("wait"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 30 {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Wait must be between 1 and 30 seconds"))
		}
		wait = parsed
	}
	scope := hubscope.NativeRequestScope(c)
	key := d.NotificationKey(scope.Organization)
	d.Cursor(key)
	authorize := func() error {
		ctx := c.Request().Context()
		now := d.Now().UTC()
		return d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
				var lookup *accessstore.LookupError
				if errors.Is(err, accessstore.ErrDenied) && errors.As(err, &lookup) && lookup.Step == "credential" {
					return hubscope.RunnerUnauthorized()
				}
				return err
			}
			identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
			if err != nil {
				return err
			}
			if !runnerfleet.RunnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, scope.Credential.ID, scope.Credential.Runner.MachineID, now) || now.Before(identity.LastHeartbeatAt) || !now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) || !slices.Contains(identity.Operations, runnerauth.Claim) {
				return tracker.ErrMachineNotFound
			}
			allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
			if err != nil {
				return err
			}
			if !allowed {
				return accessstore.ErrDenied
			}
			return nil
		})
	}
	if err := authorize(); err != nil {
		return d.HTTPError(c, err)
	}
	subscription, cancel := d.Subscribe(key)
	defer cancel()
	select {
	case <-subscription.Closed():
		return c.NoContent(http.StatusServiceUnavailable)
	default:
	}
	cursor := d.Cursor(key)
	if c.QueryParam("after") == cursor {
		timer := d.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case <-c.Request().Context().Done():
			return c.Request().Context().Err()
		case <-subscription.Closed():
			return c.NoContent(http.StatusServiceUnavailable)
		case <-timer.C:
		case <-subscription.Wake():
		}
		if err := authorize(); err != nil {
			return d.HTTPError(c, err)
		}
	}
	return c.JSON(http.StatusOK, struct {
		Cursor string `json:"cursor"`
	}{d.Cursor(key)})
}
