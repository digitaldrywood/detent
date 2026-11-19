package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) changeFilesService(caller hubscope.NativeScope) *changefiles.Service {
	return changefiles.New(changefiles.Deps{
		Repository: s.config.TenantRepository, Details: s.runDetailStore, CurrentTime: s.database.currentTime,
		Context: func(ctx context.Context) context.Context {
			return hubcontent.IsolateContentContext(s.runDetailContext(ctx))
		},
		ReadChange: func(ctx context.Context, scope hubapi.Scope, item, id string) (tracker.ChangeRequest, error) {
			return s.readChangeRecord(ctx, caller, item, id)
		},
		SourceAccess: func(ctx context.Context, item tracker.NativeWorkItemID) error {
			return s.authorizeChangeSource(ctx, caller, item)
		},
		ReadVersion: s.readChangeVersionRecord,
		Receipt: func(ctx context.Context, scope hubapi.Scope, operation, key, hash string) (json.RawMessage, bool, error) {
			return s.changeRecordReceipt(ctx, caller, operation, key, hash)
		},
		RecordError: hubapi.ProjectRecordError, Scope: func(c echo.Context) hubapi.Scope { return s.authorization().Scope(hubscope.NativeRequestScope(c)) },
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) }, InvalidRequest: s.invalidAPIRequest,
	})
}

func (s *Service) validateReviewBundleRecord(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, bundle tracker.ChangeReviewBundle, now time.Time) error {
	return hubapi.FromFeatureError(s.changeFilesService(scope).ValidateBundle(ctx, s.authorization().Scope(scope), change, version, bundle, now))
}
func (s *Service) changeViewedFiles(c echo.Context) error {
	return s.changeFilesService(hubscope.NativeRequestScope(c)).ViewedFiles(c)
}
func (s *Service) readChangeViewedFileRecords(ctx context.Context, scope hubscope.NativeScope, item, change, version string) ([]tracker.ChangeViewedFile, error) {
	value, err := s.changeFilesService(scope).ReadViewedFiles(ctx, s.authorization().Scope(scope), item, change, version)
	return value, hubapi.FromFeatureError(err)
}
func (s *Service) viewChangeFile(c echo.Context) error {
	return s.changeFilesService(hubscope.NativeRequestScope(c)).ViewFile(c)
}
func (s *Service) viewChangeFileRecordCommand(ctx context.Context, scope hubscope.NativeScope, item, change, version, operation string, request tracker.ViewChangeFile) (json.RawMessage, error) {
	value, err := s.changeFilesService(scope).ViewFileCommand(ctx, s.authorization().Scope(scope), item, change, version, operation, request)
	return value, hubapi.FromFeatureError(err)
}
func (s *Service) changeSource(c echo.Context) error {
	return s.changeFilesService(hubscope.NativeRequestScope(c)).Source(c)
}

func (s *Service) authorizeChangeSource(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID) error {
	if scope.Credential.Scope != hubapi.APIScopeWorker && scope.Credential.Runner.RunnerID == "" {
		return nil
	}
	var lease leasestore.Lease
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(ctx, scope.Project, item)
		if err != nil {
			return err
		}
		if row.OrganizationID != scope.Organization || row.ProjectID != scope.Project || row.NativeID != item {
			return corestore.ErrNotFound
		}
		lease, err = r.ActiveLeaseForIssue(ctx, row.ID)
		return err
	})
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	_, err = s.validateRunnerLeaseTenant(ctx, scope, tracker.LeaseID(lease.LeaseID), tracker.FencingToken(lease.FencingToken))
	return hubapi.ProjectRecordError(err)
}

func (s *Service) verifyChangeSource(ctx context.Context, source tracker.ChangeSource, bundle []byte) error {
	return hubapi.FromFeatureError(changefiles.VerifySource(ctx, s.sourceVerification, source, bundle))
}
