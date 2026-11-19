package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		if organization == "" || s.config.TenantRepository == nil {
			return attachment.ArchiveReference{}, hubcontent.ErrContentStorageUnconfigured
		}
		var ref attachment.ArchiveReference
		err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(hubrecords.AttemptArchiveReader)
			if !ok {
				return hubcontent.ErrContentStorageUnconfigured
			}
			object, err := reader.ArchiveObjectByKey(ctx, key)
			if err != nil {
				return err
			}
			ref = object.Reference
			if object.OrganizationID != organization || ref.Key != key || ref.Validate(string(organization)) != nil || ref.CustomerOwned || ref.BindingID != "" || ref.SchemaVersion != 1 {
				return artifact.ErrIntegrity
			}
			return nil
		})
		return ref, err
	}
	return context.WithValue(ctx, hubcontent.ChangeContentKey{}, &hubcontent.ChangeContent{Encoder: &s.archiveEncoder, Call: call, Store: store, Register: s.registerChangeContent, ReadOrganization: string(organization), ReadReference: readReference})
}

func (s *Service) changeContentMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.contentContext(c.Request().Context())))
		return next(c)
	}
}
