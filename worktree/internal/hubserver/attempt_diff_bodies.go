package hubserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerAttemptDiffBodyRoutes(e *echo.Echo) {
	e.POST("/internal/v1/diff-bodies/exists", s.attemptDiffBodyExists)
}

func diffBodyService(c echo.Context) bool {
	claims, ok := hostedSharedClaims(c)
	return ok && claims.Kind == cloudassert.KindService
}

func (s *Service) attemptDiffBodyExists(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if !diffBodyService(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	claims, _ := hostedSharedClaims(c)
	parts := strings.Split(input.Key, "/")
	if len(parts) != 7 || parts[1] != claims.Audience {
		return s.nativeAPIError(c, nativeNotFound())
	}
	exists, err := s.manifestDiffBodyExists(ctx, input.Key)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"exists": exists})
}

func (s *Service) manifestDiffBodyExists(ctx context.Context, key string) (bool, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 7 {
		return false, nil
	}
	ref := diffbody.Reference{Key: key, SHA256: parts[6], Bytes: 1}
	if ref.Validate(parts[1], parts[3], 1) != nil {
		return false, nil
	}
	if s.config.TenantRepository == nil {
		return false, nativeUnavailable("Diff body reference storage is unavailable")
	}
	var referenced, complete bool
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(parts[1]), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		referenced, complete, err = r.AttemptDiffBodyReferenced(ctx, tracker.ProjectID(parts[3]), key)
		return err
	})
	if err != nil {
		return false, err
	}
	if !referenced && !complete {
		return false, nativeUnavailable("Diff body references are not fully indexed")
	}
	return referenced, nil
}
