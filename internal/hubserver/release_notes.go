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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var value string
	err := s.database.reader.QueryRowContext(ctx, `SELECT build_time FROM hosted_release_preferences WHERE organization_id=? AND user_id=?`, s.appOrganizationID(), appSubject(credential)).Scan(&value)
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
	seen, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New("invalid release preference time")
	}
	return seen, nil
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
			return s.nativeAPIError(c, nativeInvalid("The running release is unavailable"))
		}
		_, err := s.database.db.ExecContext(ctx, `INSERT INTO hosted_release_preferences (organization_id,user_id,version,build_time) VALUES (?,?,?,?) ON CONFLICT (organization_id,user_id) DO UPDATE SET version=excluded.version,build_time=excluded.build_time WHERE excluded.build_time>=hosted_release_preferences.build_time`, s.appOrganizationID(), appSubject(credential), build.Version, build.BuildTime.UTC().Format("2006-01-02T15:04:05.000000000Z"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	request := operatortool.ReleaseNotesArguments{Cursor: c.QueryParam("cursor")}
	if value := c.QueryParam("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 {
			return s.nativeAPIError(c, nativeInvalid("Invalid release notes limit"))
		}
		request.Limit = limit
	}
	page, err := s.readReleaseNotes(ctx, credential, request)
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.nativeAPIError(c, nativeInvalid("Invalid release notes pagination"))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}
