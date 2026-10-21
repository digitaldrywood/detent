package web

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
)

// controlProblem preserves the dashboard's existing command outcomes while
// allowing non-HTTP adapters to return only opaque application errors.
type controlProblem struct {
	status  int
	code    string
	message string
}

func (p *controlProblem) Error() string { return p.message }

func writeControlProblem(c echo.Context, err error) error {
	var problem *controlProblem
	if errors.As(err, &problem) {
		return c.JSON(problem.status, errorResponse(problem.code, problem.message))
	}
	return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Operator command is unavailable"))
}
