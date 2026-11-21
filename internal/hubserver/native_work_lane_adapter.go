package hubserver

import (
	"context"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemchange"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
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
	return viewstore.StatusLine(card)
}

func (s *Service) readNativeRecovery(ctx context.Context, scope hubscope.NativeScope, item string, projected ...bool) (tracker.NativeRecovery, error) {
	ctx = s.contentContext(s.runDetailContext(ctx))
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return s.readNativeRecoveryTenant(ctx, scope, item)
}

type recoveryEvidenceCursor = workitemread.RecoveryEvidenceCursor

func recoveryRecordFragment(page *tracker.NativeRecoveryPage, content []byte, offset int64, expectedDigest string) error {
	return workitemread.RecoveryRecordFragment(page, content, offset, expectedDigest)
}

func (s *Service) fitNativeRecovery(ctx context.Context, scope hubscope.NativeScope, path string, result *tracker.NativeRecovery, bodyFragments bool) error {
	return workitemread.FitRecovery(path, result, bodyFragments, workitemread.RecoveryFitDeps{
		BoundVersion: boundedChangeVersion,
		ReadBodyCursor: func() (workpage.Cursor, []byte, error) {
			_, cursor, key, err := s.readNativePageLimit(ctx, scope, path, url.Values{"view": {"recovery"}}, 1, 1)
			return cursor, key, err
		},
	})
}

func projectNativeRecovery(result *tracker.NativeRecovery) error {
	return workitemread.ProjectRecovery(result, boundedChangeReviewResult)
}

func validDependencyReason(text string) bool {
	return workitemchange.ValidDependencyReason(text)
}

func (s *Service) updateNativeDependencyReason(c echo.Context) error {
	var request tracker.UpdateDependencyReason
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	result, err := s.updateNativeDependencyReasonTenant(c.Request().Context(), scope, c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func readIssueArchiveSection(ctx context.Context, reader attachmentstore.StorageArchiveReader, scope hubscope.NativeScope, item, section string) (workitemread.ArchiveSection, error) {
	return workitemread.ReadArchiveSection(ctx, reader, scope, item, section)
}
