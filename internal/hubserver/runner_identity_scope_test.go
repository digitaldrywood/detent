package hubserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerOperationAllowed(t *testing.T) {
	t.Parallel()
	all := []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}
	for _, test := range []struct {
		name, method, path string
		operations         []string
		want               bool
	}{
		{name: "attempt diff with claim", method: http.MethodPost, path: nativeBase + "/attempts/:attempt/diff", operations: all, want: true},
		{name: "attempt diff without claim", method: http.MethodPost, path: nativeBase + "/attempts/:attempt/diff", operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Heartbeat, runnerauth.Events}},
		{name: "attempt diff preflight with claim", method: http.MethodPost, path: nativeBase + "/attempts/:attempt/diff/check", operations: all, want: true},
		{name: "attempt diff preflight with only claim", method: http.MethodPost, path: nativeBase + "/attempts/:attempt/diff/check", operations: []string{runnerauth.Claim}, want: true},
		{name: "attempt diff preflight without claim", method: http.MethodPost, path: nativeBase + "/attempts/:attempt/diff/check", operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Heartbeat, runnerauth.Events}},
		{name: "attempt diff read", method: http.MethodGet, path: nativeBase + "/attempts/:attempt/diff", operations: []string{runnerauth.Read}, want: true},
		{name: "claims need claim", method: http.MethodPost, path: nativeBase + "/claims", operations: []string{runnerauth.Read}},
		{name: "dispatch waits need claim", method: http.MethodGet, path: nativeBase + "/claims/wait", operations: []string{runnerauth.Read}},
		{name: "dispatch waits with claim", method: http.MethodGet, path: nativeBase + "/claims/wait", operations: all, want: true},
		{name: "claims with claim", method: http.MethodPost, path: nativeBase + "/claims", operations: all, want: true},
		{name: "landing barrier claim with claim", method: http.MethodPost, path: nativeBase + "/landing-barrier", operations: []string{runnerauth.Claim}, want: true},
		{name: "landing barrier claim without claim", method: http.MethodPost, path: nativeBase + "/landing-barrier", operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Heartbeat, runnerauth.Events}},
		{name: "landing barrier read", method: http.MethodGet, path: nativeBase + "/landing-barrier", operations: []string{runnerauth.Read}, want: true},
		{name: "work item change is collaboration", method: http.MethodPost, path: nativeBase + "/work-items/:item/changes", operations: []string{runnerauth.Collaborate}, want: true},
		{name: "item events need events", method: http.MethodPost, path: nativeBase + "/work-items/:item/events", operations: []string{runnerauth.Collaborate}},
		{name: "conversation events with events", method: http.MethodPost, path: nativeBase + "/conversations/:conversation/turn-events", operations: []string{runnerauth.Events}, want: true},
		{name: "conversation events without events", method: http.MethodPost, path: nativeBase + "/conversations/:conversation/turn-events", operations: []string{runnerauth.Collaborate}},
		{name: "conversation events wrong method", method: http.MethodDelete, path: nativeBase + "/conversations/:conversation/turn-events", operations: all},
		{name: "conversation command remains forbidden", method: http.MethodPost, path: nativeBase + "/conversations/:conversation/commands", operations: all},
		{name: "heartbeat", method: http.MethodPost, path: nativeBase + "/machines/:machine/heartbeat", operations: []string{runnerauth.Heartbeat}, want: true},
		{name: "outside the native base", method: http.MethodPost, path: "/api/v2/organizations/:organization/members", operations: all},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c := echo.New().NewContext(httptest.NewRequest(test.method, "/", nil), httptest.NewRecorder())
			c.SetPath(test.path)
			if got := runnerOperationAllowed(c, test.operations); got != test.want {
				t.Fatalf("runnerOperationAllowed(%s %s, %v) = %t, want %t", test.method, test.path, test.operations, got, test.want)
			}
		})
	}
}
