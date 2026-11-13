package hubserver

import (
	"context"
	"net/url"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCursor = workpage.Cursor
type nativeIssuePageCursor = workpage.IssuePosition

func (s *Service) workPageService() *workpage.Service {
	return workpage.New(workpage.Deps{Repository: s.config.AccessRepository, Keys: s.config.SecretKeys, Now: s.config.now})
}
func (s *Service) readNativePageLimit(ctx context.Context, scope nativeScope, path string, params url.Values, maximum, defaultLimit int) (int, nativeCursor, []byte, error) {
	caller := workpage.Caller{Organization: scope.organization, CredentialID: scope.credential.ID, HostedRole: scope.credential.HostedRole, PrincipalID: scope.actor().PrincipalID, Hosted: scope.credential.Hosted != nil}
	if scope.credential.Hosted != nil {
		caller.SessionID = scope.credential.Hosted.SessionID
	}
	limit, cursor, key, err := s.workPageService().ReadLimit(s.runDetailContext(ctx), caller, path, params, maximum, defaultLimit)
	return limit, cursor, key, hubapi.FromFeatureError(err)
}
func (s *Service) readNativeCursorKey(ctx context.Context, organization tracker.OrganizationID) ([]byte, error) {
	key, err := s.workPageService().ReadKey(ctx, organization)
	return key, hubapi.FromFeatureError(err)
}
func newNativeCursorSecret(cfg Config, organization tracker.OrganizationID) (accessstore.Secret, accessstore.SecretAudit, error) {
	return workpage.NewSecret(cfg.SecretKeys, cfg.now, organization)
}
func encodeNativeCursor(cursor nativeCursor, key []byte) (string, error) {
	return workpage.Encode(cursor, key)
}
