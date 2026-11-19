package changeapi

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *HTTP) ReviewChange(c echo.Context) error {
	var request tracker.ReviewChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	return s.deps.ChangeRecordEvidenceMutation(c, "review", c.Param("version"), "", request)
}

func (s *HTTP) SubmitChangeCheck(c echo.Context) error {
	var request tracker.SubmitChangeCheck
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	if _, hosted := s.deps.HostedOrganization(); hosted && hubscope.NativeRequestScope(c).Credential.Runner.RunnerID == "" && request.Source != "independent" {
		return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
	}
	if s.deps.Repository == nil {
		return s.deps.NativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.deps.ChangeCheckRecordMutation(c, request)
}

func (s *HTTP) DiscussChange(c echo.Context) error {
	var request tracker.DiscussChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	return s.deps.ChangeRecordEvidenceMutation(c, "discussion", request.VersionID, "", request)
}
