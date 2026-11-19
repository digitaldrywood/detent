package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
)

const (
	maxSavedWorkViewBytes      = boardview.MaxSavedViewBytes
	maxSavedWorkViewsPageSize  = 25
	maxSavedWorkViewsPageBytes = 256 << 10
)

type savedWorkViewsPage = boardview.SavedViewPage
type savedWorkState = boardview.SavedViewState
type savedWorkView = boardview.SavedView

func (s *Service) savedWorkViews(c echo.Context) error {
	return s.savedWorkViewsRecord(c)
}

func validSavedWorkView(view savedWorkView) bool {
	return boardview.ValidSavedView(view)
}
