package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestHumanQuestionMigrationRequestScope(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body, project string }{
		{name: "malformed", body: "{"},
		{name: "missing project", body: `{}`},
		{name: "cross-project", body: `{"project_id":"other"}`, project: "selected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			c := e.NewContext(request, httptest.NewRecorder())
			c.SetParamNames("project_id")
			c.SetParamValues(tt.project)
			server := &Server{}
			err := server.apiMigrateHumanQuestion(c)
			var httpError *echo.HTTPError
			if !errors.As(err, &httpError) || httpError.Code != http.StatusBadRequest {
				t.Fatalf("scope error = %v", err)
			}
		})
	}
}
