package hubserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestReleaseDeliveryStatusPostgresMissingItem(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{TenantRepository: repository, Logger: slog.Default()}}
	request := httptest.NewRequest(http.MethodGet, "/release-pick", nil)
	response := httptest.NewRecorder()
	c := echo.New().NewContext(request, response)
	c.SetParamNames("item")
	c.SetParamValues("wi_missing")
	c.Set("native_scope", nativeScope{organization: tracker.OrganizationID("org_missing"), project: tracker.ProjectID("prj_missing")})
	if err := service.getReleaseDeliveryStatus(c); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
