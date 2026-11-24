package changefiles

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (s *Service) Source(c echo.Context) error {
	ctx := c.Request().Context()
	scope := s.deps.Scope(c)
	item := c.Param("item")
	change, err := s.deps.ReadChange(ctx, scope, item, c.Param("change"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	if err := s.deps.SourceAccess(ctx, tracker.NativeWorkItemID(item)); err != nil {
		return s.deps.Error(c, err)
	}
	row, err := s.deps.ReadVersion(ctx, scope.Organization, change.ID, c.Param("version"))
	if err != nil {
		return s.deps.Error(c, s.deps.RecordError(err))
	}
	content := eventstore.NewContent(s.deps.Details())
	version, err := content.Version(ctx, row)
	if err != nil {
		return s.deps.Error(c, err)
	}
	if version.Source == nil {
		return s.deps.Error(c, hubapi.NotFound())
	}
	bundle, err := content.SourceBundle(ctx, scope.Organization, row, *version.Source)
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.Blob(http.StatusOK, "application/x-git-bundle", bundle)
}

func VerifySource(ctx context.Context, slot chan struct{}, source tracker.ChangeSource, bundle []byte) error {
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := workspace.VerifyChangeSource(ctx, source, bundle); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return hubapi.Rejection(hubapi.Invalid(err.Error()), "source_verification", "source")
	}
	return nil
}
