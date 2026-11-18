package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func authenticatedRunner(c echo.Context) (hubapi.APICredential, error) {
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.Runner.RunnerID == "" || credential.Runner.RunnerID != c.Param("runner") || string(credential.Runner.OrganizationID) != c.Param("organization") {
		return hubapi.APICredential{}, hubapi.NativeNotFound()
	}
	return credential, nil
}

func (s *Service) getRunnerIdentity(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	credential, err := authenticatedRunner(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	identity, err := s.readRunnerIdentityTenant(ctx, tracker.OrganizationID(c.Param("organization")), c.Param("runner"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if identity.Binding != credential.Runner.Binding {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, identity)
}

func (s *Service) renewRunnerIdentity(c echo.Context) error {
	var request struct{}
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.changeRunnerCredential(c, "")
}

func (s *Service) rotateRunnerIdentity(c echo.Context) error {
	var request runnerauth.Rotation
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if !runnerauth.ValidCredential(request.Credential) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("A host-generated replacement credential is required"))
	}
	return s.changeRunnerCredential(c, request.Credential)
}

func (s *Service) changeRunnerCredential(c echo.Context, replacement string) error {
	credential, err := authenticatedRunner(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if apikey.HashToken(replacement) == credential.Hash {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Rotation requires a different credential"))
	}
	identity, err := s.changeRunnerCredentialTenant(c.Request().Context(), credential, replacement)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, identity)
}

func (s *Service) changeRunnerCredentialTenant(ctx context.Context, credential hubapi.APICredential, replacement string) (runnerauth.Identity, error) {
	organization := credential.Runner.OrganizationID
	if s.config.TenantRepository == nil || organization == "" || credential.ID == "" {
		return runnerauth.Identity{}, hubscope.RunnerUnauthorized()
	}
	updated := runnerauth.Identity{}
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.RunnerIdentity(ctx, credential.Runner.RunnerID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubscope.RunnerUnauthorized()
			}
			return err
		}
		if record.RemovedAt != nil || record.TokenID != credential.ID || record.OrganizationID != organization || record.RunnerID != credential.Runner.RunnerID {
			return hubscope.RunnerUnauthorized()
		}
		current, err := w.CredentialByHash(ctx, credential.Hash)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubscope.RunnerUnauthorized()
			}
			return err
		}
		now := s.config.Clock()
		if current.ID != credential.ID || current.Scope != string(hubapi.APIScopeWorker) || current.Kind != "service" || current.ExpiresAt == nil || !current.ExpiresAt.After(current.CreatedAt) || !validRunnerCredentialForRenewal(current, credential.Hash, now, replacement == "") || current.UserID != "" || current.MembershipID != "" {
			return hubscope.RunnerUnauthorized()
		}
		if replacement != "" && !record.ExpiresAt.After(now) {
			return hubscope.RunnerUnauthorized()
		}
		if record.MachineID != credential.Runner.MachineID || record.Scope != credential.Runner.Scope || !slices.Equal(record.Operations, credential.Runner.Operations) {
			return hubscope.RunnerUnauthorized()
		}
		newHash := current.Hash
		kind := "renewed"
		if replacement != "" {
			newHash, kind = apikey.HashToken(replacement), "rotated"
		}
		expires := now.Add(runnerauth.CredentialTTL)
		if err := w.RenewAccessCredential(ctx, current.ID, newHash, hubapi.TokenFingerprint(newHash), expires, now); err != nil {
			return err
		}
		record.ExpiresAt = expires
		if err := w.UpdateRunnerIdentity(ctx, record); err != nil {
			return err
		}
		if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: organization, RunnerID: record.RunnerID, ActorID: current.ID, Kind: kind, OccurredAt: now}); err != nil {
			return err
		}
		updated, err = runnerIdentityRecord(ctx, w, record)
		return err
	})
	if err != nil {
		return runnerauth.Identity{}, err
	}
	return updated, nil
}

func (s *Service) revokeRunnerIdentity(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return s.nativeAPIError(c, hubscope.RunnerUnauthorized())
	}
	_, err := s.revokeRunnerIdentityCommand(c.Request().Context(), hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("runner"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) revokeRunnerIdentityCommand(ctx context.Context, scope hubscope.NativeScope, resource string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.requireRunnerAdministrationTenant(ctx, w, scope); err != nil {
			return err
		}
		return s.revokeRunnerIdentityTenant(ctx, w, scope.Organization, resource, scope.Credential.ID, func() time.Time { return s.config.Clock().UTC() })
	})
	if err != nil {
		return nil, err
	}
	if s.notifications != nil {
		s.notifications.notify(dispatchNotificationKey(scope.Organization))
	}
	return struct{}{}, nil
}

func (s *Service) revokeRunnerIdentityTenant(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, resource, actorID string, clock func() time.Time) error {
	identity, err := w.RunnerIdentity(ctx, resource)
	if err != nil {
		return err
	}
	if identity.RemovedAt != nil {
		return nil
	}
	now := clock()
	busy, err := w.RunnerHasActiveLease(ctx, resource, now)
	if err != nil {
		return err
	}
	if busy {
		return hubapi.NativePublicInvalid("This runner has active work. Drain it and wait for its leases to finish or expire before removing it.")
	}
	credential, err := w.AccessCredential(ctx, identity.TokenID)
	if err != nil {
		return err
	}
	if credential.RevokedAt == nil {
		if err := w.RevokeAccessCredential(ctx, identity.TokenID, now); err != nil {
			return err
		}
		if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: organization, RunnerID: resource, ActorID: actorID, Kind: "revoked", OccurredAt: now}); err != nil {
			return err
		}
	}
	if err := w.RemoveRunnerIdentity(ctx, resource, now); err != nil {
		return err
	}
	return s.applyRunnerTenantUsage(ctx, w, now, resource, true, false)
}

type nativeMachineHeartbeatRequest = runnerauth.MachineReport

func (s *Service) heartbeatNativeMachine(c echo.Context) error {
	var request nativeMachineHeartbeatRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.heartbeatNativeMachineTenant(c, request)
}

func validRunnerPlatform(os, architecture string) bool {
	return (os == "" || policy.ValidToken(os)) && (architecture == "" || policy.ValidToken(architecture))
}
