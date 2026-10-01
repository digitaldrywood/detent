package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type archiveIssueRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func (s *Service) archiveNativeIssue(c echo.Context) error { return s.setNativeArchive(c, true) }
func (s *Service) restoreNativeIssue(c echo.Context) error { return s.setNativeArchive(c, false) }

func (s *Service) setNativeArchive(c echo.Context, archived bool) error {
	var request archiveIssueRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.setNativeArchiveCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request, archived)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
