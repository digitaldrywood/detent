package hubserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedNativeRejectionMessages(t *testing.T) {
	t.Parallel()
	t.Run("phase name is private", func(t *testing.T) {
		now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
		err := validateNativeRuntime(&tracker.NativeRuntimeObservation{Phase: "completed", Phases: []tracker.NativePhase{{Name: "private-phase", StartedAt: now}}})
		var failure *hubapi.NativeError
		if !errors.As(err, &failure) || strings.Contains(failure.Error(), "private-phase") || strings.Contains(failure.Message, "private-phase") {
			t.Fatal("invalid phase name was accepted or exposed")
		}
	})
	for _, test := range []struct {
		name, category, field, message, want string
	}{
		{"owner", "owner_validation", "data.finalization", "Finalizer version must name its recorded head", "Finalizer version must name its recorded head"},
		{"runtime", "runtime_validation", "data.runtime.activity", "Activity profile does not match its runtime observation", "Activity profile does not match its runtime observation"},
		{"source", "source_verification", "source", "sentinel-private", "publication rejected: category=source_verification field=source [private error text omitted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{config: Config{Hosted: &hostedplans.HostedConfig{}, Logger: discardLogger()}}
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), response)
			if err := service.nativeAPIError(c, nativeRejection(hubapi.NativeInvalid(test.message), test.category, test.field)); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			var failure struct {
				Message string
				Details struct{ Rejection tracker.PublicationRejection }
			}
			decodeHubResponse(t, response, &failure)
			if failure.Message != test.want || failure.Details.Rejection.Category != test.category || failure.Details.Rejection.Field != test.field || strings.Contains(response.Body.String(), "sentinel-private") {
				t.Fatalf("hosted refusal = %s", response.Body.String())
			}
		})
	}
}
