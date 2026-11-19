package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

const changeBase = hubapi.NativeBase + "/work-items/:item/changes"

func changeCheckRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == changeBase+"/:change/versions/:version/checks"
}

func (s *Service) registerChangeRoutes(e *echo.Echo) {
	read := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	write := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	operator := s.requireNativeScope(hubapi.APIScopeOperator)
	e.GET(hubapi.NativeBase+"/change-review-policy", s.getChangeReviewPolicy, read)
	e.PUT(hubapi.NativeBase+"/change-review-policy", s.approveChangeReviewPolicy, s.requireChangeReviewPolicyAdmin())
	e.GET(changeBase, s.listChanges, read)
	e.POST(changeBase, s.createChange, write)
	e.GET(changeBase+"/:change", s.getChange, read)
	e.POST(changeBase+"/:change/versions", s.publishChangeVersion, write)
	e.GET(changeBase+"/:change/versions/:version/source", s.changeSource, read)
	e.POST(changeBase+"/:change/discussion", s.discussChange, write)
	e.POST(changeBase+"/:change/versions/:version/reviews", s.reviewChange, s.requireChangeReviewer())
	e.GET(changeBase+"/:change/versions/:version/viewed-files", s.changeViewedFiles, operator)
	e.POST(changeBase+"/:change/versions/:version/viewed-files", s.viewChangeFile, operator)
	e.POST(changeBase+"/:change/versions/:version/checks", s.submitChangeCheck, write)
	e.POST(changeBase+"/:change/versions/:version/landing", s.landChange, write)
}

func (s *Service) changeHostedOrganization() (string, bool) {
	if s.config.Hosted == nil {
		return "", false
	}
	return s.config.Hosted.OrganizationID, true
}
