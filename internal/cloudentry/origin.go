package cloudentry

import (
	"github.com/digitaldrywood/detent/internal/cloudentry/origin"
	"github.com/labstack/echo/v4"
)

func (s *Service) originPolicy() *origin.Service {
	return origin.New(origin.Deps{PublicURL: s.config.PublicURL, Secure: s.secure})
}

func (s *Service) boundary(next echo.HandlerFunc) echo.HandlerFunc {
	return s.originPolicy().Boundary(next)
}

func (s *Service) sameOrigin(c echo.Context) bool {
	return s.originPolicy().SameOrigin(c)
}

func (s *Service) cookieName(name string) string {
	return s.originPolicy().CookieName(name)
}
