package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
)

func (s *Service) contentContext(ctx context.Context) context.Context {
	if ctx.Value(hubcontent.ChangeContentKey{}) != nil {
		return ctx
	}
	call := s.storageArchiveContent()
	store := s.runDetailStore()
	if call == nil && store == nil {
		return ctx
	}
	organization := s.accessOrganization()
	readReference := func(ctx context.Context, key string) (attachment.ArchiveReference, error) {
		return changefiles.ReadContentReference(ctx, s.config.TenantRepository, organization, key)
	}
	return context.WithValue(ctx, hubcontent.ChangeContentKey{}, &hubcontent.ChangeContent{Encoder: &s.archiveEncoder, Call: call, Store: store, Register: s.registerChangeContent, ReadOrganization: string(organization), ReadReference: readReference})
}

func (s *Service) changeContentMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.contentContext(c.Request().Context())))
		return next(c)
	}
}
