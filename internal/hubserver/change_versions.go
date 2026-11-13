package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) publishChangeVersion(c echo.Context) error {
	return s.publishChangeVersionRecord(c)
}

type preparedChangeVersion struct {
	change     tracker.ChangeRequest
	version    tracker.ChangeVersion
	raw        string
	associated bool
	delivery   tracker.ReleaseDelivery
	source     []byte
	sourceRef  attachment.ArchiveReference
	session    changeSessionAuthority
}
