package hubserver

import (
	"context"
	"encoding/json"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) runDetailStore() attachment.DetailStore {
	if s.config.RunDetails != nil {
		return s.config.RunDetails
	}
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		return s.config.Hosted.SharedEntry.RunDetails
	}
	return nil
}

func (s *Service) runDetailContext(ctx context.Context) context.Context {
	ctx = s.contentContext(ctx)
	if ctx.Value(hubcontent.RunDetailContextKey{}) != nil {
		return ctx
	}
	owner := s.runDetailStore()
	if owner == nil && s.storageArchiveContent() == nil {
		return ctx
	}
	organization := ""
	if s.config.Hosted != nil {
		organization = s.config.Hosted.OrganizationID
	}
	return context.WithValue(ctx, hubcontent.RunDetailContextKey{}, &hubcontent.RunDetailContext{Store: owner, Organization: organization, Cache: make(map[string]map[string]tracker.NativeRunData)})
}

func (s *Service) runDetailMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.runDetailContext(c.Request().Context())))
		return next(c)
	}
}

func decodeRunHistory(ctx context.Context, q hubapi.NativeQueryer, raw string, result *tracker.CollaborationData) error {
	ctx = hubrecords.PayloadQueryContext(ctx, q)
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), result); err != nil {
		return err
	}
	if result.Run == nil {
		return nil
	}
	var envelope struct {
		Run json.RawMessage `json:"run"`
	}
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), &envelope); err != nil {
		return err
	}
	return hubrecords.DecodeRunDetail(ctx, q, string(envelope.Run), result.Run)
}

type storedRunCheckpoint = hubrecords.StoredRunCheckpoint

type checkpointPayload = hubrecords.CheckpointPayload

func (s *Service) freshRunDetailContext(ctx context.Context) context.Context {
	return s.runDetailContext(context.WithValue(context.WithValue(ctx, hubcontent.RunDetailContextKey{}, nil), hubcontent.ChangeContentKey{}, nil))
}
