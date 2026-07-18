package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestStopRunAPIRequiresAuthorizationAndConfirmation(t *testing.T) {
	t.Parallel()
	stopper := &fakeRunStopper{}
	server := newStopRunServer(t, stopper)

	unauthorized := performStopRunJSON(t, server.Handler(), `/api/v1/projects/detent/runs/0/stop`, `{"issue_id":"issue-1311","confirm":true}`, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d; body = %s", unauthorized.Code, http.StatusUnauthorized, unauthorized.Body.String())
	}

	unconfirmed := performStopRunJSON(t, server.Handler(), `/api/v1/projects/detent/runs/0/stop`, `{"issue_id":"issue-1311"}`, "Bearer detent_test_token")
	if unconfirmed.Code != http.StatusPreconditionRequired {
		t.Fatalf("unconfirmed status = %d, want %d; body = %s", unconfirmed.Code, http.StatusPreconditionRequired, unconfirmed.Body.String())
	}
	if stopper.calls != 0 {
		t.Fatalf("StopRun calls = %d, want 0", stopper.calls)
	}
}

func TestStopRunAPITargetsExactActiveIdentity(t *testing.T) {
	t.Parallel()
	stopper := &fakeRunStopper{result: orchestrator.StopRunResult{ProjectID: "detent", IssueID: "issue-1311", Attempt: 0, WorkAttemptID: 1311, DetentSessionID: 91, ProviderSessionID: "thread-1311", Destination: "Todo", Priority: 2, PriorityName: "High", Reason: "make room for the release blocker", Outcome: "pending"}}
	server := newStopRunServer(t, stopper)

	recorder := performStopRunJSON(t, server.Handler(), `/api/v1/projects/detent/runs/0/stop`, `{"issue_id":"issue-1311","work_attempt_id":1311,"detent_session_id":91,"provider_session_id":"thread-1311","destination":"Todo","priority":2,"reason":"make room for the release blocker","confirm":true}`, "Bearer detent_test_token")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if stopper.request != (orchestrator.StopRunRequest{ProjectID: "detent", IssueID: "issue-1311", Attempt: 0, WorkAttemptID: 1311, DetentSessionID: 91, ProviderSessionID: "thread-1311", Destination: "Todo", Priority: 2, Reason: "make room for the release blocker"}) {
		t.Fatalf("request = %#v", stopper.request)
	}
	var result orchestrator.StopRunResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if result.Outcome != "pending" || result.Destination != "Todo" || result.PriorityName != "High" || result.Reason == "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestStopRunAPIMapsStaleAndTransitionFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		result     orchestrator.StopRunResult
		wantStatus int
		wantCode   string
	}{
		{name: "stale", err: orchestrator.ErrStopRunStale, wantStatus: http.StatusConflict, wantCode: "stale_run"},
		{name: "invalid destination", err: orchestrator.ErrStopRunInvalidRoute, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_stop_destination"},
		{name: "transition failure", err: errors.Join(orchestrator.ErrStopRunTransition, errors.New("tracker unavailable")), result: orchestrator.StopRunResult{Destination: "Blocked", Outcome: "transition_failed"}, wantStatus: http.StatusBadGateway, wantCode: "tracker_transition_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopper := &fakeRunStopper{result: tt.result, err: tt.err}
			server := newStopRunServer(t, stopper)
			recorder := performStopRunJSON(t, server.Handler(), `/api/v1/projects/detent/runs/0/stop`, `{"issue_id":"issue-1311","confirm":true}`, "Bearer detent_test_token")
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tt.wantCode) {
				t.Fatalf("body missing %q: %s", tt.wantCode, recorder.Body.String())
			}
		})
	}
}

func TestStopRunControlsRenderContextualConfirmationAndResult(t *testing.T) {
	t.Parallel()
	stopper := &fakeRunStopper{result: orchestrator.StopRunResult{ProjectID: "detent", IssueID: "issue-1311", Attempt: 0, WorkAttemptID: 1311, DetentSessionID: 91, ProviderSessionID: "thread-1311", Destination: "Blocked", Outcome: "pending"}}
	server := newStopRunServer(t, stopper)

	fleet := stopRunRequest(t, server.Handler(), http.MethodGet, "/fleet", "", map[string]string{})
	for _, want := range []string{"Stop run and route item", "grid-cols-1", "md:grid-cols-[minmax(0,1.6fr)_130px_150px_minmax(0,1fr)_90px_36px]", "/api/v1/projects/detent/runs/0/stop"} {
		if !strings.Contains(fleet.Body.String(), want) {
			t.Fatalf("fleet missing %q: %s", want, fleet.Body.String())
		}
	}

	session := stopRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/board/session?project=detent&issue=issue-1311", "", map[string]string{"Authorization": "Bearer detent_test_token"})
	for _, want := range []string{"Stop run", "min-h-9", "/api/v1/projects/detent/runs/0/stop"} {
		if !strings.Contains(session.Body.String(), want) {
			t.Fatalf("board session missing %q: %s", want, session.Body.String())
		}
	}

	dialogPath := "/api/v1/projects/detent/runs/0/stop?issue_id=issue-1311&work_attempt_id=1311&detent_session_id=91&provider_session_id=thread-1311"
	dialog := stopRunRequest(t, server.Handler(), http.MethodGet, dialogPath, "", map[string]string{"Authorization": "Bearer detent_test_token", "HX-Request": "true"})
	for _, want := range []string{"digitaldrywood/detent", "digitaldrywood/detent#1311", "code", "In Progress", "Detent 91", "provider thread-1311", "attempt 0", "Blocked", "Backlog", "Cancelled", "Todo", "Urgent · rank 1", "Low · rank 4", `maxlength="280"`, "sm:grid-cols-2", "hx-disabled-elt=", "stop-run-submit-indicator", "Stopping...", "Cancel"} {
		if !strings.Contains(dialog.Body.String(), want) {
			t.Fatalf("confirmation missing %q: %s", want, dialog.Body.String())
		}
	}

	form := url.Values{"issue_id": {"issue-1311"}, "work_attempt_id": {"1311"}, "detent_session_id": {"91"}, "provider_session_id": {"thread-1311"}, "confirm": {"true"}}
	success := stopRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/runs/0/stop", form.Encode(), map[string]string{"Authorization": "Bearer detent_test_token", "Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true"})
	wantTrigger := `{"kanbanActionSucceeded":{"message":"Stop accepted; board routing is continuing in the background."}}`
	if success.Code != http.StatusOK || success.Header().Get("HX-Trigger") != wantTrigger {
		t.Fatalf("success status/header = %d/%q; body = %s", success.Code, success.Header().Get("HX-Trigger"), success.Body.String())
	}
	if !strings.Contains(success.Body.String(), `data-stop-run-result="pending"`) || !strings.Contains(success.Body.String(), "board routing to Blocked is continuing") {
		t.Fatalf("success result missing: %s", success.Body.String())
	}
}

func TestStopRunHTMXTransitionFailureStaysActionable(t *testing.T) {
	t.Parallel()
	stopper := &fakeRunStopper{result: orchestrator.StopRunResult{Destination: "Blocked", Outcome: "transition_failed"}, err: errors.Join(orchestrator.ErrStopRunTransition, errors.New("tracker unavailable"))}
	server := newStopRunServer(t, stopper)
	form := url.Values{"issue_id": {"issue-1311"}, "work_attempt_id": {"1311"}, "detent_session_id": {"91"}, "provider_session_id": {"thread-1311"}, "confirm": {"true"}}
	recorder := stopRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/runs/0/stop", form.Encode(), map[string]string{"Authorization": "Bearer detent_test_token", "Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	for _, want := range []string{"Redispatch remains suppressed", "tracker unavailable", "Retry move to Blocked", `data-stop-run-result="transition_failed"`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("failure result missing %q: %s", want, recorder.Body.String())
		}
	}
}

func newStopRunServer(t *testing.T, stopper *fakeRunStopper) *web.Server {
	t.Helper()
	deps := testDeps(t)
	deps.RunStopper = stopper
	if err := deps.Hub.Publish(telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{{ID: "issue-1311", Identifier: "digitaldrywood/detent#1311", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/1311", Title: "Stop unhealthy run", State: "In Progress"}},
		Running:     []telemetry.Running{{Issue: telemetry.Issue{ID: "issue-1311", Identifier: "digitaldrywood/detent#1311", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/1311", Title: "Stop unhealthy run", State: "In Progress", RuntimeIdentity: agentidentity.Identity{Role: "code"}}, Attempt: 0, WorkAttemptID: 1311, DetentSessionID: 91, SessionID: "thread-1311", StopDestination: "Blocked", StopPriorityOptions: []telemetry.StopRunPriorityOption{{Rank: 1, Name: "Urgent"}, {Rank: 2, Name: "High"}, {Rank: 3, Name: "Medium"}, {Rank: 4, Name: "Low"}}}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_test_token"}, StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func performStopRunJSON(t *testing.T, handler http.Handler, path string, body string, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if authorization != "" {
		headers["Authorization"] = authorization
	}
	return stopRunRequest(t, handler, http.MethodPost, path, body, headers)
}

func stopRunRequest(t *testing.T, handler http.Handler, method string, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

type fakeRunStopper struct {
	request orchestrator.StopRunRequest
	result  orchestrator.StopRunResult
	err     error
	calls   int
}

func (s *fakeRunStopper) StopRun(_ context.Context, request orchestrator.StopRunRequest) (orchestrator.StopRunResult, error) {
	s.calls++
	s.request = request
	return s.result, s.err
}
