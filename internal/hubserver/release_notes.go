package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/releasenotes"
)

type releaseNotesBuild struct {
	releasenotes.Build
	Running bool `json:"running"`
	Unread  bool `json:"unread"`
}

type releaseNotesPage struct {
	Builds     []releaseNotesBuild `json:"builds"`
	HasUnread  bool                `json:"has_unread"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

func (s *Service) runningRelease() (releasenotes.Build, bool) {
	for _, build := range s.releaseNotes {
		if build.Version == releasenotes.Version(s.config.Version) {
			return build, true
		}
	}
	return releasenotes.Build{}, false
}

func (s *Service) releaseSeenTime(ctx context.Context, credential apiCredential) (time.Time, error) {
	repository, ok := s.config.AccessRepository.(accountstore.Repository)
	if !ok {
		return time.Time{}, errors.New("account repository unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var preference accountstore.ReleasePreference
	var err error
	if credential.Hosted != nil {
		preference, err = repository.ReleasePreference(ctx, s.accessOrganization(), credential.Hosted.Subject, credential.HostedMembership, credential.ID)
	} else {
		preference, err = repository.LocalReleasePreference(ctx, s.accessOrganization(), credential.ID, credential.Hash, s.config.now())
	}
	if errors.Is(err, sql.ErrNoRows) {
		build, ok := s.runningRelease()
		if !ok {
			return time.Time{}, nil
		}
		return build.BuildTime, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return preference.BuildTime, nil
}

func (s *Service) releasesUnread(seen time.Time) bool {
	if seen.IsZero() {
		return false
	}
	for _, build := range s.releaseNotes {
		if build.BuildTime.After(seen) {
			return true
		}
	}
	return false
}

func (s *Service) readReleaseNotes(ctx context.Context, credential apiCredential, request operatortool.ReleaseNotesArguments) (releaseNotesPage, error) {
	page := releaseNotesPage{Builds: []releaseNotesBuild{}}
	if request.Limit < 0 || request.Limit > 100 || len(request.Cursor) > 512 {
		return page, operatortool.ErrInvalidArguments
	}
	limit := request.Limit
	if limit == 0 {
		limit = 20
	}
	start := 0
	if request.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(request.Cursor)
		if err != nil {
			return page, operatortool.ErrInvalidArguments
		}
		start = -1
		for i, build := range s.releaseNotes {
			if build.Version == string(raw) {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return page, operatortool.ErrInvalidArguments
		}
	}
	seen, err := s.releaseSeenTime(ctx, credential)
	if err != nil {
		return page, err
	}
	page.HasUnread = s.releasesUnread(seen)
	end := min(start+limit, len(s.releaseNotes))
	for _, build := range s.releaseNotes[start:end] {
		page.Builds = append(page.Builds, releaseNotesBuild{Build: build, Running: build.Version == releasenotes.Version(s.config.Version), Unread: !seen.IsZero() && build.BuildTime.After(seen)})
	}
	if end < len(s.releaseNotes) {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(s.releaseNotes[end-1].Version))
	}
	return page, nil
}

func (s *Service) releaseNotesAPI(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if c.Param("organization") != s.appOrganizationID() {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if c.Request().Method == http.MethodPut {
		build, ok := s.runningRelease()
		if !ok {
			return s.nativeAPIError(c, nativePublicInvalid("The running release is unavailable"))
		}
		repository, ok := s.config.AccessRepository.(accountstore.Repository)
		if !ok {
			return s.nativeAPIError(c, errors.New("account repository unavailable"))
		}
		preference := accountstore.ReleasePreference{Version: build.Version, BuildTime: build.BuildTime.UTC()}
		if credential.Hosted != nil {
			err = repository.SetReleasePreference(ctx, s.accessOrganization(), credential.Hosted.Subject, credential.HostedMembership, credential.ID, preference)
		} else {
			err = repository.SetLocalReleasePreference(ctx, s.accessOrganization(), credential.ID, credential.Hash, s.config.now(), preference)
		}
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	request := operatortool.ReleaseNotesArguments{Cursor: c.QueryParam("cursor")}
	if value := c.QueryParam("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid release notes limit"))
		}
		request.Limit = limit
	}
	page, err := s.readReleaseNotes(ctx, credential, request)
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid release notes pagination"))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}
