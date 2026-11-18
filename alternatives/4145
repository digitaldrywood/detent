package hubserver

import (
	"context"
	"net/url"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCursor = workpage.Cursor
type nativeIssuePageCursor = workpage.IssuePosition

func (s *Service) workPageService() *workpage.Service {
	return workpage.New(workpage.Deps{Repository: s.config.AccessRepository, Keys: s.config.SecretKeys, Now: s.config.Clock})
}
func (s *Service) readNativePageLimit(ctx context.Context, scope hubscope.NativeScope, path string, params url.Values, maximum, defaultLimit int) (int, nativeCursor, []byte, error) {
	caller := workpage.Caller{Organization: scope.Organization, CredentialID: scope.Credential.ID, HostedRole: scope.Credential.HostedRole, PrincipalID: scope.Actor().PrincipalID, Hosted: scope.Credential.Hosted != nil}
	if scope.Credential.Hosted != nil {
		caller.SessionID = scope.Credential.Hosted.SessionID
	}
	limit, cursor, key, err := s.workPageService().ReadLimit(s.runDetailContext(ctx), caller, path, params, maximum, defaultLimit)
	return limit, cursor, key, hubapi.FromFeatureError(err)
}
func (s *Service) readNativeCursorKey(ctx context.Context, organization tracker.OrganizationID) ([]byte, error) {
	key, err := s.workPageService().ReadKey(ctx, organization)
	return key, hubapi.FromFeatureError(err)
}
func newNativeCursorSecret(cfg Config, organization tracker.OrganizationID) (accessstore.Secret, accessstore.SecretAudit, error) {
	return workpage.NewSecret(cfg.SecretKeys, cfg.Clock, organization)
}
func encodeNativeCursor(cursor nativeCursor, key []byte) (string, error) {
	return workpage.Encode(cursor, key)
}
