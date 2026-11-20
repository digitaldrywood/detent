package hubserver

import (
	"github.com/labstack/echo/v4"

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
