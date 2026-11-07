package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/onboardingpage"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func onboardingViewer(credential apiCredential) (projectstore.OnboardingViewer, string, error) {
	v := projectstore.OnboardingViewer{ID: credential.ID, Hash: credential.Hash, SessionHash: credential.SessionHash, Scope: string(credential.Scope), NativeOnly: credential.NativeOnly, UserID: hostedSubject(credential), Membership: credential.HostedMembership, Principal: credential.HostedPrincipal, Role: credential.HostedRole, KeyScope: string(credential.HostedKeyScope), KeyKind: credential.HostedKeyKind, ProjectAccess: string(credential.HostedProjectAccess), RunnerID: credential.Runner.RunnerID}
	if credential.EntryKey != nil {
		v.EntryKind, v.EntryAccess = credential.EntryKey.Kind, credential.EntryKey.Access
		v.EntryID, v.EntryBinding, v.EntryPermission = credential.EntryKey.ID, credential.EntryKey.CredentialBinding, string(credential.EntryKey.Permission)
		v.Projects = credential.EntryKey.Projects
		v.BlockedProjects = credential.EntryKey.BlockedProjects
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, "", fmt.Errorf("encode onboarding viewer: %w", err)
	}
	hash := sha256.Sum256(raw)
	return v, hex.EncodeToString(hash[:]), nil
}

func onboardingCredential(v projectstore.OnboardingViewer) apiCredential {
	c := apiCredential{ID: v.ID, Hash: v.Hash, SessionHash: v.SessionHash, Scope: apiScope(v.Scope), NativeOnly: v.NativeOnly, HostedMembership: v.Membership, HostedPrincipal: v.Principal, HostedRole: v.Role, HostedKeyScope: apikey.Scope(v.KeyScope), HostedKeyKind: v.KeyKind, HostedProjectAccess: hostedProjectAccess(v.ProjectAccess)}
	c.Runner.RunnerID = v.RunnerID
	if v.UserID != "" {
		c.Hosted = &auth.HostedIdentity{Subject: v.UserID}
	}
	if v.EntryKind != "" {
		c.EntryKey = &apikey.KeyAuthority{ProjectContext: apikey.ProjectContext{Access: v.EntryAccess, Projects: v.Projects}, ID: v.EntryID, CredentialBinding: v.EntryBinding, Permission: apikey.Scope(v.EntryPermission), Kind: v.EntryKind, BlockedProjects: v.BlockedProjects}
	}
	return c
}

func (s *Service) OnboardingPageMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "onboarding-pages", Bytes: integrationPageMaintenanceBytes, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		return s.maintainOnboardingPage(ctx, tracker.OrganizationID(tenant.Organization))
	}}
}

func (s *Service) maintainOnboardingPage(ctx context.Context, organization tracker.OrganizationID) error {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), integrationPageMaintenanceBytes); err != nil {
		return err
	}
	if _, ok := s.runDetailStore().(*hubmaintenance.Details); !ok {
		return errors.New("maintenance requires byte-limited payload storage")
	}
	var candidate projectstore.OnboardingCandidate
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.OnboardingPageWriter)
		if !ok {
			return errors.New("onboarding page storage unavailable")
		}
		var err error
		candidate, err = writer.ClaimOnboardingPage(ctx)
		return err
	})
	if err != nil || candidate.ProjectID == "" {
		return err
	}
	value, err := s.projectOnboardingRecord(ctx, nativeScope{organization: organization, project: candidate.ProjectID, credential: onboardingCredential(candidate.Viewer)})
	var native *nativeError
	if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, corestore.ErrNotFound) || errors.As(err, &native) && native.Code == "not_found" {
		return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			writer, ok := w.(projectstore.OnboardingPageWriter)
			if !ok {
				return errors.New("onboarding page storage unavailable")
			}
			return writer.DiscardOnboardingPage(ctx, candidate)
		})
	}
	if err != nil {
		return err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.OnboardingPageWriter)
		if !ok {
			return errors.New("onboarding page storage unavailable")
		}
		return writer.PublishOnboardingPage(ctx, candidate, onboardingpage.Summary(value), s.config.now())
	})
	if err == nil && s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project onboarding page", slog.String("organization_id", string(organization)), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.config.now().Sub(candidate.SourceAt)))
	}
	return err
}

func (s *Service) getOnboardingPage(c echo.Context) error {
	var value onboarding.Project
	var cold bool
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization"))}
	err := s.readWorkItemPageSnapshot(c.Request().Context(), scope, func(ctx context.Context) error {
		if err := s.resolveNativeRequestScope(ctx, c); err != nil {
			return err
		}
		scope = nativeRequestScope(c)
		if scope.credential.Scope == apiScopeWorker {
			var err error
			value, err = s.projectOnboarding(ctx, scope)
			return err
		}
		_, key, err := onboardingViewer(scope.credential)
		if err != nil {
			return err
		}
		return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			reader, ok := r.(projectstore.OnboardingPageReader)
			if !ok {
				return errors.New("onboarding page storage unavailable")
			}
			var err error
			value, err = reader.OnboardingPage(ctx, scope.project, key)
			if errors.Is(err, corestore.ErrNotFound) {
				cold = true
				return nil
			}
			return err
		})
	})
	if err == nil && cold {
		value, err = s.projectOnboarding(c.Request().Context(), scope)
		if err == nil {
			viewer, key, viewerErr := onboardingViewer(scope.credential)
			if viewerErr != nil {
				return s.nativeAPIError(c, viewerErr)
			}
			queueErr := s.config.TenantRepository.WriteTenant(c.Request().Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := s.authorizeAccessScope(c.Request().Context(), w, scope, false); err != nil {
					return err
				}
				writer, ok := w.(projectstore.OnboardingPageWriter)
				if !ok {
					return errors.New("onboarding page storage unavailable")
				}
				return writer.RequestOnboardingPage(c.Request().Context(), scope.project, key, viewer)
			})
			if errors.Is(queueErr, accessstore.ErrDenied) || errors.Is(queueErr, auth.ErrHostedIdentity) {
				err = queueErr
			} else if queueErr != nil && s.config.Logger != nil {
				s.config.Logger.WarnContext(c.Request().Context(), "request onboarding page preparation", "error", queueErr)
			}
		}
		value = onboardingpage.Summary(value)
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	if scope.credential.Hosted != nil && (!hostedReadRequest(c) || scope.credential.Hosted.SupportActor != "") {
		if err := s.hostedAudit(c.Request().Context(), scope.credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, value)
}
