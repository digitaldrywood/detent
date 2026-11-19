package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type teamReference = threadstore.Reference
type teamThreadInput = operatortool.ThreadArguments
type teamParticipant = threadstore.Participant
type teamThread = threadstore.Thread
type teamPost = threadstore.Post
type teamThreadPage = threadstore.Page
type teamThreadRead = threadstore.Read

func (s *Service) registerTeamThreadRoutes(e *echo.Echo) {
	threads.RegisterRoutes(e.Group(""), s.threadHTTPDeps())
}

func (s *Service) teamThreadOperation(ctx context.Context, name string, in teamThreadInput) (any, error) {
	value, err := threads.NewHTTP(s.threadHTTPDeps()).Operation(ctx, name, in)
	return value, hubapi.FromFeatureError(err)
}
