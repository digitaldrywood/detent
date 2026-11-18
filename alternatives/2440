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

func TestNativeRuntimePhaseRejectionReason(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		phase  tracker.NativePhase
		reason string
	}{
		{name: "invalid name", phase: tracker.NativePhase{Name: "private-phase", StartedAt: started}, reason: "invalid_name"},
		{name: "missing start", phase: tracker.NativePhase{Name: "implementation"}, reason: "missing_start"},
		{name: "finish before start", phase: tracker.NativePhase{Name: "implementation", StartedAt: started, FinishedAt: started.Add(-time.Second)}, reason: "finish_before_start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateNativeRuntime(&tracker.NativeRuntimeObservation{Phase: "completed", Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: started}, test.phase}})
			var failure *hubapi.NativeError
			if !errors.As(err, &failure) || failure == nil {
				t.Fatalf("validateNativeRuntime() error = %v, want nativeError", err)
			}
			want := []any{"phase_index", 1, "phase_failure", test.reason}
			if len(failure.LogAttributes) != len(want) {
				t.Fatalf("phase log attributes = %v, want %v", failure.LogAttributes, want)
			}
			for i := range want {
				if failure.LogAttributes[i] != want[i] {
					t.Errorf("phase log attribute %d = %v, want %v", i, failure.LogAttributes[i], want[i])
				}
			}
			if strings.Contains(strings.Join([]string{failure.Error(), failure.Message}, " "), "private-phase") {
				t.Fatal("phase rejection exposed the phase name")
			}
		})
	}
}

func TestHostedNativeRejectionMessages(t *testing.T) {
	t.Parallel()
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
