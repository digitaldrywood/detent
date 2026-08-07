package web

import (
	_ "embed"
	"net/http"

	"github.com/labstack/echo/v4"
)

const (
	openAPIPath      = "/api/v1/openapi.yaml"
	openAPIMediaType = "application/vnd.oai.openapi+yaml;version=3.0"
	openAPIVersion   = "1.0.0"
)

//go:embed openapi.yaml
var openAPIDocument []byte

func (s *Server) openAPI(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=300")
	c.Response().Header().Set("X-Detent-OpenAPI-Version", openAPIVersion)
	return c.Blob(http.StatusOK, openAPIMediaType, openAPIDocument)
}
