package releasefeed

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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/releasenotes"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Build struct {
	releasenotes.Build
	Running bool `json:"running"`
	Unread  bool `json:"unread"`
}

type Page struct {
	Builds     []Build `json:"builds"`
	HasUnread  bool    `json:"has_unread"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type Viewer struct {
	Hosted       bool
	Subject      string
	Membership   string
	CredentialID string
	Hash         string
}

type Deps struct {
	Builds          []releasenotes.Build
	Version         string
	Accounts        accountstore.Repository
	Organization    tracker.OrganizationID
	Now             func() time.Time
	Viewer          func(echo.Context) (Viewer, error)
	CredentialError func(echo.Context, error) error
	AppOrganization func() string
	Error           func(echo.Context, error) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) Running() (releasenotes.Build, bool) {
	for _, build := range s.deps.Builds {
		if build.Version == releasenotes.Version(s.deps.Version) {
			return build, true
		}
	}
	return releasenotes.Build{}, false
}

func (s *Service) SeenTime(ctx context.Context, viewer Viewer) (time.Time, error) {
	repository := s.deps.Accounts
	if repository == nil {
		return time.Time{}, errors.New("account repository unavailable")
	}
	var preference accountstore.ReleasePreference
	var err error
	if viewer.Hosted {
		preference, err = repository.ReleasePreference(ctx, s.deps.Organization, viewer.Subject, viewer.Membership, viewer.CredentialID)
	} else {
		preference, err = repository.LocalReleasePreference(ctx, s.deps.Organization, viewer.CredentialID, viewer.Hash, s.deps.Now())
	}
	if errors.Is(err, sql.ErrNoRows) {
		build, ok := s.Running()
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

func (s *Service) Unread(seen time.Time) bool {
	if seen.IsZero() {
		return false
	}
	for _, build := range s.deps.Builds {
		if build.BuildTime.After(seen) {
			return true
		}
	}
	return false
}

func (s *Service) Read(ctx context.Context, viewer Viewer, request operatortool.ReleaseNotesArguments) (Page, error) {
	page := Page{Builds: []Build{}}
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
		for i, build := range s.deps.Builds {
			if build.Version == string(raw) {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return page, operatortool.ErrInvalidArguments
		}
	}
	seen, err := s.SeenTime(ctx, viewer)
	if err != nil {
		return page, err
	}
	page.HasUnread = s.Unread(seen)
	end := min(start+limit, len(s.deps.Builds))
	for _, build := range s.deps.Builds[start:end] {
		page.Builds = append(page.Builds, Build{Build: build, Running: build.Version == releasenotes.Version(s.deps.Version), Unread: !seen.IsZero() && build.BuildTime.After(seen)})
	}
	if end < len(s.deps.Builds) {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(s.deps.Builds[end-1].Version))
	}
	return page, nil
}

func (s *Service) API(c echo.Context) error {
	ctx := c.Request().Context()
	viewer, err := s.deps.Viewer(c)
	if err != nil {
		return s.deps.CredentialError(c, err)
	}
	if c.Param("organization") != s.deps.AppOrganization() {
		return s.deps.Error(c, hubapi.NotFound())
	}
	if c.Request().Method == http.MethodPut {
		build, ok := s.Running()
		if !ok {
			return s.deps.Error(c, hubapi.PublicInvalid("The running release is unavailable"))
		}
		repository := s.deps.Accounts
		if repository == nil {
			return s.deps.Error(c, errors.New("account repository unavailable"))
		}
		preference := accountstore.ReleasePreference{Version: build.Version, BuildTime: build.BuildTime.UTC()}
		if viewer.Hosted {
			err = repository.SetReleasePreference(ctx, s.deps.Organization, viewer.Subject, viewer.Membership, viewer.CredentialID, preference)
		} else {
			err = repository.SetLocalReleasePreference(ctx, s.deps.Organization, viewer.CredentialID, viewer.Hash, s.deps.Now(), preference)
		}
		if err != nil {
			return s.deps.Error(c, err)
		}
	}
	request := operatortool.ReleaseNotesArguments{Cursor: c.QueryParam("cursor")}
	if value := c.QueryParam("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 {
			return s.deps.Error(c, hubapi.PublicInvalid("Invalid release notes limit"))
		}
		request.Limit = limit
	}
	page, err := s.Read(ctx, viewer, request)
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.deps.Error(c, hubapi.PublicInvalid("Invalid release notes pagination"))
	}
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, page)
}
