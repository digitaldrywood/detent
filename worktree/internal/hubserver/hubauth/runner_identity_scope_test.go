package hubauth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
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
		{name: "attempt diff with claim", method: http.MethodPost, path: hubauth.NativeBase + "/attempts/:attempt/diff", operations: all, want: true},
		{name: "attempt diff without claim", method: http.MethodPost, path: hubauth.NativeBase + "/attempts/:attempt/diff", operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Heartbeat, runnerauth.Events}},
		{name: "attempt diff preflight with claim", method: http.MethodPost, path: hubauth.NativeBase + "/attempts/:attempt/diff/check", operations: all, want: true},
		{name: "attempt diff preflight with only claim", method: http.MethodPost, path: hubauth.NativeBase + "/attempts/:attempt/diff/check", operations: []string{runnerauth.Claim}, want: true},
		{name: "attempt diff preflight without claim", method: http.MethodPost, path: hubauth.NativeBase + "/attempts/:attempt/diff/check", operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Heartbeat, runnerauth.Events}},
		{name: "attempt diff read", method: http.MethodGet, path: hubauth.NativeBase + "/attempts/:attempt/diff", operations: []string{runnerauth.Read}, want: true},
		{name: "claims need claim", method: http.MethodPost, path: hubauth.NativeBase + "/claims", operations: []string{runnerauth.Read}},
		{name: "dispatch waits need claim", method: http.MethodGet, path: hubauth.NativeBase + "/claims/wait", operations: []string{runnerauth.Read}},
		{name: "dispatch waits with claim", method: http.MethodGet, path: hubauth.NativeBase + "/claims/wait", operations: all, want: true},
		{name: "claims with claim", method: http.MethodPost, path: hubauth.NativeBase + "/claims", operations: all, want: true},
		{name: "work item change is collaboration", method: http.MethodPost, path: hubauth.NativeBase + "/work-items/:item/changes", operations: []string{runnerauth.Collaborate}, want: true},
		{name: "work item body needs collaboration", method: http.MethodPatch, path: hubauth.NativeBase + "/work-items/:item", operations: []string{runnerauth.Read}},
		{name: "work item body with collaboration", method: http.MethodPatch, path: hubauth.NativeBase + "/work-items/:item", operations: all, want: true},
		{name: "item events need events", method: http.MethodPost, path: hubauth.NativeBase + "/work-items/:item/events", operations: []string{runnerauth.Collaborate}},
		{name: "conversation events with events", method: http.MethodPost, path: hubauth.NativeBase + "/conversations/:conversation/turn-events", operations: []string{runnerauth.Events}, want: true},
		{name: "conversation events without events", method: http.MethodPost, path: hubauth.NativeBase + "/conversations/:conversation/turn-events", operations: []string{runnerauth.Collaborate}},
		{name: "conversation events wrong method", method: http.MethodDelete, path: hubauth.NativeBase + "/conversations/:conversation/turn-events", operations: all},
		{name: "conversation command remains forbidden", method: http.MethodPost, path: hubauth.NativeBase + "/conversations/:conversation/commands", operations: all},
		{name: "heartbeat", method: http.MethodPost, path: hubauth.NativeBase + "/machines/:machine/heartbeat", operations: []string{runnerauth.Heartbeat}, want: true},
		{name: "repository report", method: http.MethodPost, path: hubauth.NativeBase + "/onboarding/repositories/report", operations: []string{runnerauth.Heartbeat}, want: true},
		{name: "repository report without heartbeat", method: http.MethodPost, path: hubauth.NativeBase + "/onboarding/repositories/report", operations: []string{runnerauth.Read}},
		{name: "outside the native base", method: http.MethodPost, path: "/api/v2/organizations/:organization/members", operations: all},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c := echo.New().NewContext(httptest.NewRequest(test.method, "/", nil), httptest.NewRecorder())
			c.SetPath(test.path)
			if got := hubauth.RunnerOperationAllowed(c, test.operations); got != test.want {
				t.Fatalf("runnerOperationAllowed(%s %s, %v) = %t, want %t", test.method, test.path, test.operations, got, test.want)
			}
		})
	}
}
