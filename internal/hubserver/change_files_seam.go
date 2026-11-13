package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func (s *Service) changeFilesService(caller nativeScope) *changefiles.Service {
	return changefiles.New(changefiles.Deps{
		Repository: s.config.TenantRepository, Details: s.runDetailStore, CurrentTime: s.database.currentTime,
		Context: func(ctx context.Context) context.Context { return isolateContentContext(s.runDetailContext(ctx)) },
		ReadChange: func(ctx context.Context, scope hubapi.Scope, item, id string) (tracker.ChangeRequest, error) {
			return s.readChangeRecord(ctx, caller, item, id)
		},
		ReadVersion: s.readChangeVersionRecord,
		Receipt: func(ctx context.Context, scope hubapi.Scope, operation, key, hash string) (json.RawMessage, bool, error) {
			return s.changeRecordReceipt(ctx, caller, operation, key, hash)
		},
		RecordError: projectRecordError, Scope: func(c echo.Context) hubapi.Scope { return s.featureScope(nativeRequestScope(c)) },
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) }, InvalidRequest: s.invalidAPIRequest,
	})
}

func (s *Service) validateReviewBundleRecord(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, bundle tracker.ChangeReviewBundle, now time.Time) error {
	return fromFeatureError(s.changeFilesService(scope).ValidateBundle(ctx, s.featureScope(scope), change, version, bundle, now))
}
func (s *Service) changeViewedFiles(c echo.Context) error {
	return s.changeFilesService(nativeRequestScope(c)).ViewedFiles(c)
}
func (s *Service) readChangeViewedFileRecords(ctx context.Context, scope nativeScope, item, change, version string) ([]tracker.ChangeViewedFile, error) {
	value, err := s.changeFilesService(scope).ReadViewedFiles(ctx, s.featureScope(scope), item, change, version)
	return value, fromFeatureError(err)
}
func (s *Service) viewChangeFile(c echo.Context) error {
	return s.changeFilesService(nativeRequestScope(c)).ViewFile(c)
}
func (s *Service) viewChangeFileRecordCommand(ctx context.Context, scope nativeScope, item, change, version, operation string, request tracker.ViewChangeFile) (json.RawMessage, error) {
	value, err := s.changeFilesService(scope).ViewFileCommand(ctx, s.featureScope(scope), item, change, version, operation, request)
	return value, fromFeatureError(err)
}
func (s *Service) changeSource(c echo.Context) error {
	return s.changeFilesService(nativeRequestScope(c)).Source(c)
}
func (s *Service) verifyChangeSource(ctx context.Context, source tracker.ChangeSource, bundle []byte) error {
	return fromFeatureError(changefiles.VerifySource(ctx, s.sourceVerification, source, bundle))
}
