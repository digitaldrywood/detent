package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestRunnerProjectDiagnosticOwner(t *testing.T) {
	var logs bytes.Buffer
	f := newDefaultNativeFixture(t, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
	r.enroll(t)
	now := f.service.config.now()
	issue := f.create(t, "rejected observation")
	event := nativeStartedEvent(tracker.NativeLease{ID: "lease_unknown", FencingToken: 1, PolicyID: hubTestPolicy().ID})
	event.Type = "run.observed"
	event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", LocalAttemptID: 5, Generation: 9, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 99}}
	event.Data.CompletionBody = "sentinel-secret-customer"
	server := httptest.NewServer(f.service.echo)
	defer server.Close()
	client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return r.redemption.Credential }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = native.AppendEvent(t.Context(), issue.WorkItemID, event)
	if err == nil {
		t.Fatal("invalid observation was accepted")
	}
	var rejection *hubclient.APIError
	if !errors.As(err, &rejection) || rejection.Status != http.StatusUnprocessableEntity || rejection.Rejection == nil || rejection.Rejection.Category != "runtime_validation" || rejection.Rejection.Field != "data.runtime.activity" {
		t.Fatalf("rejection=%+v error=%v", rejection, err)
	}
	if strings.Contains(err.Error(), "sentinel") {
		t.Fatal("public error disclosed private request")
	}
	for _, test := range []struct {
		name, field string
		body        map[string]any
	}{
		{"unknown field", "", map[string]any{"sentinel-secret-customer": "private body"}},
		{"wrong type", "schema_version", map[string]any{"schema_version": "sentinel-secret-customer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, test.body)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if strings.Contains(response.Body.String(), "sentinel-secret-customer") {
				t.Fatal("decoder leaked rejected body")
			}
			var failure hubclient.APIError
			decodeHubResponse(t, response, &failure)
			if failure.Rejection == nil || failure.Rejection.Category != "body_decode" || failure.Rejection.Field != test.field {
				t.Fatalf("decoder attribution=%+v", failure.Rejection)
			}
		})
	}

	var accepted int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type LIKE 'run.%'").Scan(&accepted); err != nil || accepted != 0 {
		t.Fatalf("rejected event fabricated history: %d %v", accepted, err)
	}

	view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Records: []runnerauth.DiagnosticAttempt{{Key: "local:5", IssueID: "wi_unmatched", LocalAttemptID: 5, Membership: []string{"durable_active"}, NativeAttemptID: event.Data.AttemptID, HostOperation: "append.run.observed", OperationPending: true, OperationObservedAt: now, LatestErrorCode: "invalid_request", LatestError: rejection.Rejection.DiagnosticError(), Unavailable: map[string]string{"native_attempt_id": "unrecorded"}}}, Admissions: []runnerauth.DiagnosticAdmission{{IssueID: "wi_ready", ObservedAt: now, Result: "skipped", Predicate: "project_capacity_full", Unavailable: map[string]string{}}}, Counts: map[string]int{"durable_active": 1}, Unavailable: map[string]string{"running_build": "unavailable"}}}
	post := func() {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
	}
	post()
	second := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	second.enroll(t)
	ambiguous := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?issue_id=wi_unmatched&limit=20", r.redemption.Credential, nil)
	failure := requireNativeCode(t, ambiguous, http.StatusUnprocessableEntity, "invalid_request")
	if failure.Message != "Multiple runners report this project; select runner_id" {
		t.Fatalf("ambiguous diagnostic=%+v", failure)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID+"&issue_id=wi_unmatched", r.redemption.Credential, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var workerPage runnerauth.DiagnosticPage
	decodeHubResponse(t, response, &workerPage)
	if len(workerPage.Records) != 1 || workerPage.Records[0].LocalAttemptID != 5 || workerPage.Records[0].LatestError != "publication rejected: category=runtime_validation field=data.runtime.activity [private error text omitted]" || workerPage.Records[0].NativeAttemptID != event.Data.AttemptID || !workerPage.Records[0].OperationObservedAt.Equal(now) {
		t.Fatalf("worker diagnostic=%+v", workerPage)
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/diagnostic-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/diagnostic-context-test", testHubAdminToken, nil), http.StatusOK)
	ctx := operatortool.BindConnection(<-contexts, "diagnostic-test", "fixture")
	executor := hubProjectExecutor{f.service}
	if err := executor.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	call := func(ctx context.Context, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
		raw, err := json.Marshal(args)
		if err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.RunnerProjectDiagnostics, Arguments: raw})
		if err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		if strings.Contains(string(result.Content), "secret") || strings.Contains(string(result.Content), "customer") {
			t.Fatal("private error disclosed")
		}
		var page runnerauth.DiagnosticPage
		err = json.Unmarshal(result.Content, &page)
		return page, err
	}
	args := operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID, Limit: 1}
	ambiguousArgs := args
	ambiguousArgs.RunnerID = ""
	var refusal *operatortool.RequestError
	if _, err := call(ctx, ambiguousArgs); !errors.As(err, &refusal) || refusal.Code != "invalid_request" || refusal.Message != failure.Message {
		t.Fatalf("ambiguous MCP diagnostic=%v", err)
	}
	secondArgs := args
	secondArgs.RunnerID = second.binding.RunnerID
	if page, err := call(ctx, secondArgs); err != nil || page.RunnerID != second.binding.RunnerID {
		t.Fatalf("second runner diagnostic=%+v err=%v", page, err)
	}
	var before string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT project_configuration_json || routing_settings_json || cast(revision AS TEXT) || last_heartbeat_at FROM runner_identities WHERE id = ?", r.binding.RunnerID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		page, err := call(ctx, args)
		if err != nil || page.Status != "available" || len(page.Admissions) != 1 || page.NextCursor == "" {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		next := args
		next.Cursor = page.NextCursor
		page, err = call(ctx, next)
		if err != nil || len(page.Records) != 1 || page.Records[0].LocalAttemptID != 5 || page.Records[0].LatestError != workerPage.Records[0].LatestError || page.Records[0].NativeAttemptID != event.Data.AttemptID {
			t.Fatalf("next=%+v err=%v", page, err)
		}
	}
	var after string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT project_configuration_json || routing_settings_json || cast(revision AS TEXT) || last_heartbeat_at FROM runner_identities WHERE id = ?", r.binding.RunnerID).Scan(&after); err != nil || before != after {
		t.Fatalf("read mutated runner: %v", err)
	}
	connection := operatortool.CurrentConnection(ctx)
	resolve := connection.Resolve
	connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
		authority, err := resolve(ctx)
		authority.Check = func(context.Context, operatortool.Requirement) error { return operatortool.ErrAccessDenied }
		return authority, err
	}
	if _, err := call(operatortool.WithConnection(ctx, connection), args); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("scope denial=%v", err)
	}
	other := args
	other.ProjectID = "prj_unauthorized"
	if _, err := call(ctx, other); err == nil {
		t.Fatal("unauthorized project read succeeded")
	}
	view.Diagnostics.Records[0].LatestError = "sentinel-secret-customer"
	post()
	unknown, err := call(ctx, operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID, AttemptID: event.Data.AttemptID})
	if err != nil || len(unknown.Records) != 1 || unknown.Records[0].LatestError != "[private error text omitted]" || unknown.Records[0].Unavailable["publication_rejection"] != "unavailable" {
		t.Fatalf("unclassified rejection=%+v %v", unknown, err)
	}
	view.Diagnostics.ObservedAt = now.Add(-runnerauth.HeartbeatTimeout - time.Second)
	post()
	page, err := call(ctx, args)
	if err != nil || page.Status != "stale" {
		t.Fatalf("stale=%+v %v", page, err)
	}
	view.Diagnostics = nil
	post()
	page, err = call(ctx, args)
	if err != nil || page.Status != "unavailable" || page.Unavailable["snapshot"] == "" {
		t.Fatalf("older=%+v %v", page, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", formatHubTime(now.Add(-runnerauth.HeartbeatTimeout-time.Minute)), r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	page, err = call(ctx, args)
	if err != nil || page.Status != "offline" || page.RunnerID != r.binding.RunnerID {
		t.Fatalf("offline=%+v %v", page, err)
	}
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-diagnostics")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign.base+"/runner-diagnostics", foreign.token, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign.base+"/runner-diagnostics", r.redemption.Credential, nil), http.StatusNotFound)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE id = ?", formatHubTime(now), r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID, r.redemption.Credential, nil), http.StatusUnauthorized)
	if strings.Contains(logs.String(), "sentinel-secret-customer") {
		t.Fatal("request secret leaked into logs")
	}
}
