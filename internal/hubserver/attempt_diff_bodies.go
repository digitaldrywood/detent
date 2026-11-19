package hubserver

import (
	"github.com/labstack/echo/v4"
)

func (s *Service) registerAttemptDiffBodyRoutes(e *echo.Echo) {
	e.POST("/internal/v1/diff-bodies/exists", s.attemptDiffBodyExists)
}
