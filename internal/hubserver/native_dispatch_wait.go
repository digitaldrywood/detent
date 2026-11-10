package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func dispatchNotificationKey(organization tracker.OrganizationID) string {
	return "dispatch:" + string(organization)
}

func (b *notificationBroker) dispatchCursor(key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	revision := b.revisions[key]
	b.revisions[key] = revision
	return fmt.Sprintf("%s:%d", b.epoch, revision)
}

func (s *Service) notifyNativeDispatch(ctx context.Context, scope nativeScope, input any) {
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Type != "run.finished" {
		return
	}
	s.notifications.notify(dispatchNotificationKey(scope.organization))
	s.stopMonthlyBudgetTurns(ctx, scope.organization)
	s.stopMonthlyBudgetSpritePasses(ctx, scope.organization)
	switch input.(type) {
	case monthlyBudgetRequest, spriteUsageRequest:
		s.resumeMonthlyBudgetWork(ctx, scope)
	case tracker.NativeRunEvent:
		if s.conversations != nil {
			if err := s.conversations.wakePending(ctx); err != nil {
				s.config.Logger.Warn("Resume cost-corrected conversations", "error", err)
			}
		}
	}
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Type == "run.finished" && scope.project != "" {
		s.startSpritePoolForQueue(scope)
	}
}

func (s *Service) signalNativeDispatch(scope nativeScope) {
	s.notifications.notify(dispatchNotificationKey(scope.organization))
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
}

func (s *Service) waitNativeCandidates(c echo.Context) error {
	wait := 30
	if value := c.QueryParam("wait"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 30 {
			return s.nativeAPIError(c, nativePublicInvalid("Wait must be between 1 and 30 seconds"))
		}
		wait = parsed
	}
	scope := nativeRequestScope(c)
	key := dispatchNotificationKey(scope.organization)
	s.notifications.dispatchCursor(key)
	authorize := func() error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		now := s.config.now().UTC()
		return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				var lookup *accessstore.LookupError
				if errors.Is(err, accessstore.ErrDenied) && errors.As(err, &lookup) && lookup.Step == "credential" {
					return runnerUnauthorized()
				}
				return err
			}
			identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
			if err != nil {
				return err
			}
			if !runnerIdentityMatchesHeldLease(identity, scope.organization, scope.credential.Runner.RunnerID, scope.credential.ID, scope.credential.Runner.MachineID, now) || now.Before(identity.LastHeartbeatAt) || !now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) || !slices.Contains(identity.Operations, runnerauth.Claim) {
				return tracker.ErrMachineNotFound
			}
			allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
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
		return s.nativeAPIError(c, err)
	}
	subscription, cancel := s.notifications.subscribe(key)
	defer cancel()
	select {
	case <-subscription.closed:
		return c.NoContent(http.StatusServiceUnavailable)
	default:
	}
	cursor := s.notifications.dispatchCursor(key)
	if c.QueryParam("after") == cursor {
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case <-c.Request().Context().Done():
			return c.Request().Context().Err()
		case <-subscription.closed:
			return c.NoContent(http.StatusServiceUnavailable)
		case <-timer.C:
		case <-subscription.wake:
		}
		if err := authorize(); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, struct {
		Cursor string `json:"cursor"`
	}{s.notifications.dispatchCursor(key)})
}
