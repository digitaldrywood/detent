package cloudentry

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
)

const entryClientShell = "app/conversation/index.html"

func (s *Service) clientFiles() fs.FS {
	if s.config.clientFS != nil {
		return s.config.clientFS
	}
	return detent.StaticFS()
}

func (s *Service) clientBuilt() bool {
	_, err := fs.Stat(s.clientFiles(), entryClientShell)
	return err == nil
}

func (s *Service) organizationHome(id string) string {
	if s.clientBuilt() {
		return "/organizations/" + id + "/work"
	}
	return "/organizations/" + id + "/organization"
}

func (s *Service) clientShell(c echo.Context) (bool, error) {
	content, err := fs.ReadFile(s.clientFiles(), entryClientShell)
	if err != nil {
		return false, nil
	}
	meta := `<meta name="detent-base-path" content=""><meta name="detent-sign-in-path" content="/"><meta name="detent-surface" content="entry">`
	shell := string(content)
	if head := strings.Index(shell, "<head>"); head >= 0 {
		shell = shell[:head+len("<head>")] + meta + shell[head+len("<head>"):]
	} else {
		shell = meta + shell
	}
	c.Response().Header().Set("Cache-Control", "no-cache")
	return true, c.HTMLBlob(http.StatusOK, []byte(shell))
}

func wantsJSON(c echo.Context) bool {
	return strings.Contains(c.Request().Header.Get(echo.HeaderAccept), echo.MIMEApplicationJSON)
}

func (s *Service) refuse(c echo.Context, status int, code, message string) error {
	if wantsJSON(c) {
		return c.JSON(status, map[string]string{"code": code, "message": message})
	}
	return s.denied(c, status, message)
}

func (s *Service) next(c echo.Context, status int, location string, body map[string]any) error {
	if wantsJSON(c) {
		if body == nil {
			body = map[string]any{}
		}
		body["next"] = location
		return c.JSON(status, body)
	}
	return c.Redirect(http.StatusSeeOther, location)
}
