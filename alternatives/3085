package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

const operatorConnectionHeader = "X-Detent-Connection-ID"

func (s *Server) apiOperatorConnection(c echo.Context) error {

	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		return err
	}
	id := "stdio-" + hex.EncodeToString(data[:])
	ctx := operatortool.BindConnection(c.Request().Context(), id, "local MCP stdio")
	if err := (dashboardOperatorExecutor{server: s}).OpenConnection(ctx); err != nil {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	return c.JSON(http.StatusCreated, struct {
		ID string `json:"connection_id"`
	}{id})
}
