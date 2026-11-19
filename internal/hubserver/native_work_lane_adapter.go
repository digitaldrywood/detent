package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
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

func boundBoardProjection(issue tracker.NativeIssue, reporter viewstore.BoardReporterRelations) (tracker.NativeIssue, tracker.NativeIssue, viewstore.BoardReporterRelations, int, error) {
	return boardview.BoundProjection(issue, reporter)
}

func boardModelCard(scope hubscope.NativeScope, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
	return boardview.ModelCard(scope, row)
}

func nativeBoardStatusLine(card tracker.NativeBoardCard) string {
	return boardview.StatusLine(card)
}
