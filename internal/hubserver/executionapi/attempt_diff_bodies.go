package executionapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d DiffDeps) diffBodyService(c echo.Context) bool {
	claims, ok := d.ServiceClaims(c)
	return ok && claims.Kind == cloudassert.KindService
}

func (d DiffDeps) AttemptDiffBodyExists(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if !d.diffBodyService(c) {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	var input struct {
		Key string `json:"key"`
	}
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return d.InvalidRequest(c, err)
	}
	claims, _ := d.ServiceClaims(c)
	parts := strings.Split(input.Key, "/")
	if len(parts) != 7 || parts[1] != claims.Audience {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	exists, err := d.ManifestDiffBodyExists(ctx, input.Key)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"exists": exists})
}

func (d DiffDeps) ManifestDiffBodyExists(ctx context.Context, key string) (bool, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 7 {
		return false, nil
	}
	ref := diffbody.Reference{Key: key, SHA256: parts[6], Bytes: 1}
	if ref.Validate(parts[1], parts[3], 1) != nil {
		return false, nil
	}
	if d.Repository == nil {
		return false, d.Unavailable("Diff body reference storage is unavailable")
	}
	var referenced, complete bool
	err := d.Repository.ReadTenant(ctx, tracker.OrganizationID(parts[1]), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		referenced, complete, err = r.AttemptDiffBodyReferenced(ctx, tracker.ProjectID(parts[3]), key)
		return err
	})
	if err != nil {
		return false, err
	}
	if !referenced && !complete {
		return false, d.Unavailable("Diff body references are not fully indexed")
	}
	return referenced, nil
}
