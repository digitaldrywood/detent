package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/releasefeed"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type releaseNotesPage = releasefeed.Page

func releaseNotesViewer(credential hubapi.APICredential) releasefeed.Viewer {
	viewer := releasefeed.Viewer{CredentialID: credential.ID, Hash: credential.Hash, Membership: credential.HostedMembership}
	if credential.Hosted != nil {
		viewer.Hosted, viewer.Subject = true, credential.Hosted.Subject
	}
	return viewer
}

func (s *Service) releaseFeed() *releasefeed.Service {
	accounts, _ := s.config.AccessRepository.(accountstore.Repository)
	return releasefeed.New(releasefeed.Deps{
		Builds: s.releaseNotes, Version: s.config.Version, Accounts: accounts, Organization: s.accessOrganization(), Now: s.config.Clock,
		Viewer: func(c echo.Context) (releasefeed.Viewer, error) {
			credential, _, err := s.appCredential(c)
			return releaseNotesViewer(credential), err
		},
		CredentialError: s.hostedAPIError, AppOrganization: s.appOrganizationID,
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
	})
}

func (s *Service) releaseSeenTime(ctx context.Context, credential hubapi.APICredential) (time.Time, error) {
	return s.releaseFeed().SeenTime(ctx, releaseNotesViewer(credential))
}

func (s *Service) releasesUnread(seen time.Time) bool { return s.releaseFeed().Unread(seen) }

func (s *Service) readReleaseNotes(ctx context.Context, credential hubapi.APICredential, request operatortool.ReleaseNotesArguments) (releaseNotesPage, error) {
	return s.releaseFeed().Read(ctx, releaseNotesViewer(credential), request)
}

func (s *Service) releaseNotesAPI(c echo.Context) error { return s.releaseFeed().API(c) }
