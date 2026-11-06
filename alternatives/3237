package hubserver

import (
	"github.com/labstack/echo/v4"
)

type workViewPreference struct {
	Query *string `json:"query"`
}

func (s *Service) workViewPreference(c echo.Context) error {
	return s.workViewPreferenceRecord(c)
}
