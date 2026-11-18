package dispatch

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDiagnosticBuildProvenance(t *testing.T) {
	now := time.Now().UTC()
	client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":["runner_project_diagnostics"]}`)), Request: request}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	source := fixtureConnector(native)

	scheduler := &Scheduler{now: func() time.Time { return now }, machine: Machine{Version: "rolling.current"}, nativeClaims: map[string]nativeClaim{}}
	current := runnerauth.BuildEvidence{Version: "rolling.current", Commit: strings.Repeat("a", 40), Source: "development", OS: "darwin", Architecture: "arm64", ObservedAt: now}
	historical := current
	historical.Version = "historical.release"
	historical.VerifiedRelease = true
	update := &runnerauth.UpdateObservation{Running: current, Receipt: &runnerauth.UpdateReceipt{Running: &historical}}
	view := runnerauth.ProjectConfiguration{Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Counts: map[string]int{}, Unavailable: map[string]string{"running_build": "unavailable"}}}
	if err := scheduler.enrichProjectDiagnostics(t.Context(), source, &view, update); err != nil {
		t.Fatal(err)
	}
	if view.Diagnostics.RunningBuild.Version != "rolling.current" || view.Diagnostics.RunningBuild.Commit != strings.Repeat("a", 40) || view.Diagnostics.RunningBuild.VerifiedRelease {
		t.Fatal("historical receipt substituted for running build")
	}
	fixtureWithoutFeature(t, client, tracker.NativeProjectDiagnosticsCapability)
	if err := scheduler.enrichProjectDiagnostics(t.Context(), source, &view, update); err != nil || view.Diagnostics != nil {
		t.Fatalf("older Hub received diagnostic extension: %v", err)
	}
}
