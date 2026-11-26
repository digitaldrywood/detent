package runnerupdate

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerenroll"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/update"
)

type targetPolicy struct {
	runner       runnerstore.UpdateTarget
	organization runnerstore.UpdateTarget
}

func (p targetPolicy) target(defaultVersion string) (string, bool) {
	if p.organization.Version != "" {
		return p.organization.Version, true
	}
	if p.runner.Version != "" {
		return p.runner.Version, true
	}
	return defaultVersion, false
}

func readTargetPolicy(ctx context.Context, r tenantstore.Reader, id string) (targetPolicy, error) {
	var policy targetPolicy
	owner, ok := r.(runnerstore.UpdateTargetReader)
	if !ok {
		return policy, nil
	}
	var err error
	policy.organization, err = owner.RunnerUpdateTarget(ctx, "")
	if err != nil || id == "" || policy.organization.Version != "" {
		return policy, err
	}
	policy.runner, err = owner.RunnerUpdateTarget(ctx, id)
	return policy, err
}

func EffectiveTarget(ctx context.Context, r tenantstore.Reader, id, defaultVersion string) (string, bool, error) {
	policy, err := readTargetPolicy(ctx, r, id)
	if err != nil {
		return "", false, err
	}
	version, held := policy.target(defaultVersion)
	return version, held, nil
}

func OfferAllowed(ctx context.Context, r tenantstore.Reader, id string, request *runnerauth.UpdateRequest) (bool, error) {
	if request == nil {
		return false, nil
	}
	target, held, err := EffectiveTarget(ctx, r, id, request.Version)
	if err != nil {
		return false, err
	}
	return offerAllowedForTarget(ctx, r, request, target, held)
}

func offerAllowedForTarget(ctx context.Context, r tenantstore.Reader, request *runnerauth.UpdateRequest, target string, held bool) (bool, error) {
	if request == nil {
		return false, nil
	}
	if held && strings.TrimPrefix(request.Version, "v") != strings.TrimPrefix(target, "v") {
		return false, nil
	}
	if owner, ok := r.(runnerstore.UpdateTargetReader); ok {
		failed, err := owner.RunnerVersionFailed(ctx, strings.TrimPrefix(request.Version, "v"))
		if err != nil {
			return false, err
		}
		if failed {
			return false, nil
		}
	}
	return true, nil
}

func ApplyAvailableRouting(ctx context.Context, reader tenantstore.Reader, runner *runnerauth.Runner, urgent runnerstore.UrgentUpdate, version string) error {
	if runner.UpdateRequest == nil && urgent.Request == nil {
		return nil
	}
	target, held, err := EffectiveTarget(ctx, reader, runner.RunnerID, "")
	if err != nil {
		return err
	}
	allowed, err := offerAllowedForTarget(ctx, reader, runner.UpdateRequest, target, held)
	if err != nil {
		return err
	}
	if !allowed {
		runner.UpdateRequest = nil
	}
	allowed, err = offerAllowedForTarget(ctx, reader, urgent.Request, target, held)
	if err != nil {
		return err
	}
	if allowed {
		ApplyUrgentRouting(runner, UrgentUpdate{Revision: urgent.Revision, Request: urgent.Request}, version)
	}
	return nil
}

func (s *Service) SetTarget(ctx context.Context, scope hubapi.Scope, id string, target runnerstore.UpdateTarget) error {
	target.Version = strings.TrimPrefix(strings.TrimSpace(target.Version), "v")
	if target.Revision < 0 {
		return hubapi.PublicInvalid("valid target revision required")
	}
	if target.Version != "" {
		if _, err := update.CompareVersions(target.Version, target.Version); err != nil {
			return hubapi.PublicInvalid("valid release version required")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		owner, ok := w.(runnerstore.UpdateTargetWriter)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		return owner.SetRunnerUpdateTarget(ctx, id, target)
	})
}

func (s *httpRoutes) setRunnerTarget(c echo.Context) error {
	credential, ok := s.deps.Credential(c)
	if !ok || credential.Runner.RunnerID != "" {
		return s.deps.Error(c, runnerenroll.Unauthorized())
	}
	var target runnerstore.UpdateTarget
	if err := hubapi.DecodeJSON(c, &target); err != nil {
		return s.deps.Invalid(c, err)
	}
	if err := s.deps.Service().SetTarget(c.Request().Context(), s.scope(c, credential), c.Param("runner"), target); err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, runnerstore.UpdateTarget{Version: target.Version, Revision: target.Revision + 1})
}

func (s *httpRoutes) getRunnerTarget(c echo.Context) error {
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, runnerenroll.Unauthorized())
	}
	scope := s.scope(c, credential)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	var target runnerstore.UpdateTarget
	err := s.deps.Service().deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		owner, ok := r.(runnerstore.UpdateTargetReader)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		var err error
		target, err = owner.RunnerUpdateTarget(ctx, c.Param("runner"))
		return err
	})
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, target)
}
