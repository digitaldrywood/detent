package hubserver

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestRunnerAdmissionPublishesNativeDecision(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "admission-history")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.enroll(t)
	first, second := f.create(t, "First delivery"), f.create(t, "Second delivery")
	foreign := newNativeFixture(t, f.service, "", "foreign-admission")
	foreignIssue := foreign.create(t, "Foreign delivery")
	now := f.service.config.now()
	view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{
		ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Counts: map[string]int{}, Unavailable: map[string]string{},
		Admissions: []runnerauth.DiagnosticAdmission{
			{IssueID: string(first.WorkItemID), WorkItemRevision: int64(first.Revision), ObservedAt: now, Result: "skipped", Predicate: "duplicate_pull_request_work"},
			{IssueID: string(second.WorkItemID), WorkItemRevision: int64(second.Revision), ObservedAt: now, Result: "skipped", Predicate: "current_head_ci_wait"},
			{IssueID: string(foreignIssue.WorkItemID), WorkItemRevision: int64(foreignIssue.Revision), ObservedAt: now, Result: "skipped", Predicate: "duplicate_pull_request_work"},
		},
	}}
	post := func() {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
	}
	post()
	post()
	call := hostedContextProtocol(t, f.service, changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID)), "http")
	for i, issue := range []tracker.NativeIssue{first, second} {
		var data tracker.CollaborationData
		var raw, eventID string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT id, data_json FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision'", issue.WorkItemID).Scan(&eventID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatal(err)
		}
		want := view.Diagnostics.Admissions[i].Predicate
		if data.Decision == nil || data.Decision.Reason != want || data.Decision.Outcome != "skipped" || data.Decision.RunnerID != r.binding.RunnerID || data.Decision.WorkItemRevision != issue.Revision || !data.Decision.At.Equal(now) {
			t.Fatalf("persisted decision = %+v", data.Decision)
		}
		payload := hostedContextData(t, call("tools/call", operatortool.ExplainItem, map[string]any{"project_id": string(f.project.ID), "reference": strconv.Itoa(issue.Number)}), false)
		var explanation explain.IssueExplanation
		if err := json.Unmarshal(payload, &explanation); err != nil {
			t.Fatal(err)
		}
		if latest := explanation.Eligibility.Latest; latest == nil || latest.Reason != want || latest.EvidenceID != eventID || latest.Outcome != "skipped" {
			t.Fatalf("explain_item lost refusal: %s", payload)
		}
	}
	view.Diagnostics.Admissions[0].Predicate = "project_capacity_full"
	view.Diagnostics.Admissions[0].WorkItemRevision++
	view.Diagnostics.Admissions[1].IssueID = "wi_unknown"
	post()
	view.Diagnostics.Admissions[0].WorkItemRevision = int64(first.Revision)
	view.Diagnostics.Admissions[0].ObservedAt = now.Add(-time.Second)
	post()
	var count int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type='scheduler.decision'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("repeated or mismatched observations changed history: count=%d error=%v", count, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, stage := range []string{"legacy", "converted"} {
		if stage == "legacy" {
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET analytics_projection_version=NULL,scheduler_outcome=NULL,scheduler_reason='',scheduler_source='',scheduler_runner_id='' WHERE organization_id=? AND project_id=? AND type='scheduler.decision'`, scope.organization, scope.project); err != nil {
				t.Fatal(err)
			}
		} else if err := f.service.convertSchedulerDecisions(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, issue := range []tracker.NativeIssue{first, second} {
			event, err := latestNativeRuntimeEvent(t.Context(), f.service.database.reader, scope, string(issue.WorkItemID), "scheduler.decision", "skipped")
			if err != nil || event.Data.Decision == nil || event.Data.Decision.RunnerID != r.binding.RunnerID {
				t.Fatalf("%s scheduler selection: %+v %v", stage, event, err)
			}
			raw, err := previousSchedulingDecision(t.Context(), f.service.database.reader, scope, string(issue.WorkItemID), *event.Data.Decision)
			if err != nil || raw == "" {
				t.Fatalf("%s scheduler deduplication: %s %v", stage, raw, err)
			}
			newer, err := hasNewerRunnerDecision(t.Context(), f.service.database.reader, scope, string(issue.WorkItemID), r.binding.RunnerID, now.Add(-time.Second))
			if err != nil || !newer {
				t.Fatalf("%s newer decision: %v %v", stage, newer, err)
			}
		}
	}

}

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
	view.ObservedAt = now.Add(-10 * time.Second)
	view.Diagnostics.ObservedAt = view.ObservedAt
	for i := 1; i < runnerauth.DiagnosticRecordLimit; i++ {
		view.Diagnostics.Records = append(view.Diagnostics.Records, runnerauth.DiagnosticAttempt{
			Key: "local:" + strconv.Itoa(i+5), IssueID: "wi_historical", LocalAttemptID: int64(i + 5),
			Membership: []string{"durable_active"}, LeaseExpiresAt: now.Add(-time.Hour),
		})
	}
	post()
	var stored runnerauth.ProjectConfiguration
	var storedJSON string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT json_extract(project_configuration_json, ?) FROM runner_identities WHERE id = ?", "$."+jsonPathKey(view.ProjectID), r.binding.RunnerID).Scan(&storedJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil || len(stored.Diagnostics.Records) != 1 {
		t.Fatalf("stored diagnostics retained historical records: %s, %v", storedJSON, err)
	}
	t.Run("expires after preparation", func(t *testing.T) {
		originalClock := f.service.database.now
		defer func() { f.service.database.now = originalClock }()
		received := now
		f.service.database.now = func() time.Time { return received }
		view.Diagnostics.Records[1].LeaseExpiresAt = now.Add(time.Second)
		defer func() { view.Diagnostics.Records[1].LeaseExpiresAt = now.Add(-time.Hour) }()
		view.Paused = true
		advanced := false
		timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
			if strings.HasPrefix(query, "SELECT r.routing_facts_ready,CASE WHEN r.routing_facts_ready IS NULL OR c.runner_id IS NOT NULL THEN r.routing_settings_json END") {
				received = now.Add(3 * time.Second)
				advanced = true
			}
		}}
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
		requireNativeStatus(t, response, http.StatusOK)
		if !advanced {
			t.Fatal("clock did not advance after preparation")
		}
		var raw, digest string
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT json_extract(r.project_configuration_json, ?), o.snapshot_digest
FROM runner_identities r JOIN runner_project_observations o ON o.runner_id = r.id AND o.organization_id = r.organization_id
WHERE r.id = ? AND o.project_id = ?`, "$."+jsonPathKey(view.ProjectID), r.binding.RunnerID, view.ProjectID).Scan(&raw, &digest); err != nil {
			t.Fatal(err)
		}
		var snapshot runnerauth.ProjectConfiguration
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Diagnostics == nil || len(snapshot.Diagnostics.Records) != 1 || snapshot.Diagnostics.Records[0].Key != "local:5" {
			t.Fatalf("stored diagnostics retained expired record: %s", raw)
		}
		if !snapshot.Paused {
			t.Fatal("stored configuration lost simultaneous pause change")
		}
		projected, err := readRunnerBoardProjection(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, r.binding.RunnerID, "wi_unmatched")
		if err != nil || projected.Diagnostics == nil || len(projected.Diagnostics.Records) != 1 || projected.Diagnostics.Records[0].Key != "local:5" {
			t.Fatalf("typed diagnostics disagree with stored snapshot: %+v %v", projected.Diagnostics, err)
		}
		var expired int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_project_diagnostic_attempts WHERE organization_id=? AND runner_id=? AND project_id=? AND record_key<>?`, f.project.OrganizationID, r.binding.RunnerID, f.project.ID, "local:5").Scan(&expired); err != nil || expired != 0 {
			t.Fatalf("typed diagnostics retained expired records: %d %v", expired, err)
		}
		wantDigest, err := projectConfigurationDigest(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if digest != wantDigest {
			t.Fatalf("stored digest = %s, want pruned snapshot digest %s", digest, wantDigest)
		}
	})
	for _, runtimeFresh := range []bool{true, true, false} {
		view.ObservedAt = now.Add(-2 * runnerauth.HeartbeatTimeout)
		view.Diagnostics.ObservedAt = view.Diagnostics.ObservedAt.Add(time.Second)
		view.Diagnostics.RuntimeObservedAt = time.Time{}
		if runtimeFresh {
			view.Diagnostics.RuntimeObservedAt = view.Diagnostics.ObservedAt
		}
		received := f.service.config.now()
		updates := 0
		timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
			if strings.HasPrefix(query, "UPDATE runner_identities") && strings.Contains(query, "project_configuration_json") {
				updates++
			}
		}}
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
		requireNativeStatus(t, response, http.StatusOK)
		if updates != 0 {
			t.Fatalf("configuration updates = %d, want 0", updates)
		}
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT json_extract(project_configuration_json, ?) FROM runner_identities WHERE id = ?", "$."+jsonPathKey(view.ProjectID), r.binding.RunnerID).Scan(&storedJSON); err != nil {
			t.Fatal(err)
		}
		stored = runnerauth.ProjectConfiguration{}
		if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil {
			t.Fatal(err)
		}
		if err := overlayProjectConfigurationObservation(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, r.binding.RunnerID, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.ObservedAt.Before(received) || !stored.Diagnostics.ObservedAt.Equal(view.Diagnostics.ObservedAt) || !stored.Diagnostics.RuntimeObservedAt.Equal(view.Diagnostics.RuntimeObservedAt) {
			t.Fatalf("stored freshness = %+v", stored)
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID, r.redemption.Credential, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page runnerauth.DiagnosticPage
		decodeHubResponse(t, response, &page)
		if page.Status != "available" || page.ReceivedAt.Before(received) || !page.ObservedAt.Equal(view.Diagnostics.ObservedAt) || !page.RuntimeObservedAt.Equal(view.Diagnostics.RuntimeObservedAt) {
			t.Fatalf("reader freshness = %+v", page)
		}
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/diagnostic-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/diagnostic-context-test", testHubAdminToken, nil), http.StatusOK)
	ctx := operatortool.BindConnection(<-contexts, "diagnostic-test", "fixture")
	executor := hubProjectExecutor{f.service}
	if err := executor.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	ownerArgs, err := json.Marshal(operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID})
	if err != nil {
		t.Fatal(err)
	}
	ownerResult, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.LocalProjectConfiguration, Arguments: ownerArgs})
	if err != nil {
		t.Fatal(err)
	}
	var owner runnerauth.ProjectConfiguration
	if err := json.Unmarshal(ownerResult.Content, &owner); err != nil || owner.RunnerID != r.binding.RunnerID || !owner.ObservedAt.Equal(stored.ObservedAt) || owner.Diagnostics == nil || !owner.Diagnostics.ObservedAt.Equal(view.Diagnostics.ObservedAt) {
		t.Fatalf("configuration owner freshness = %s, error = %v", ownerResult.Content, err)
	}
	view.Diagnostics.Records = view.Diagnostics.Records[:1]
	for _, test := range []struct {
		membership string
		expires    time.Time
	}{
		{"running", time.Time{}},
		{"claimed", time.Time{}},
		{"deferred", time.Time{}},
		{"native_claim", time.Time{}},
		{"durable_active", now.Add(time.Minute)},
	} {
		t.Run(test.membership, func(t *testing.T) {
			view.Diagnostics.Records = append(view.Diagnostics.Records[:1], runnerauth.DiagnosticAttempt{
				Key: "local:6", IssueID: "wi_live", LocalAttemptID: 6,
				Membership: []string{test.membership}, LeaseExpiresAt: test.expires,
			})
			post()
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID+"&issue_id=wi_live", r.redemption.Credential, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page runnerauth.DiagnosticPage
			decodeHubResponse(t, response, &page)
			if len(page.Records) != 1 || page.Records[0].LocalAttemptID != 6 {
				t.Fatalf("live diagnostic missing: %+v", page)
			}
		})
	}
	view.Diagnostics.Records = view.Diagnostics.Records[:1]
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
	changePath := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	created := performHubAPIRequest(t, f.service, http.MethodPost, changePath, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "diagnostic-change"}, Title: "Rejected version"})
	requireNativeStatus(t, created, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, created, &change)
	operator, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return f.token }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := operator.Native(f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := view.Diagnostics.Records[0]
	for _, test := range []struct {
		name, category, field, want string
		edit                        func(*tracker.PublishChangeVersion)
	}{
		{"receipt", "version_validation", "validation", "publication rejected: category=version_validation field=validation [private error text omitted]", func(request *tracker.PublishChangeVersion) {
			request.Validation = &gate.CommandResult{HeadSHA: "wrong-head", Output: "sentinel-secret-customer"}
		}},
		{"source metadata", "version_validation", "source", "publication rejected: category=version_validation field=source [private error text omitted]", func(request *tracker.PublishChangeVersion) {
			request.Source = &tracker.ChangeSource{Format: "sentinel-secret-customer"}
		}},
		{"source verification", "source_verification", "source", "publication rejected: category=source_verification field=source [private error text omitted]", func(request *tracker.PublishChangeVersion) {
			request.SourceBundle = changeTestSource(t, &request.ChangeVersionInput, []byte("sentinel-secret-customer"))
			request.SourceBundle[len(request.SourceBundle)-1] ^= 0xff
			request.Source.BundleSHA256 = tracker.ChangeSourceDigest(request.SourceBundle)
		}},
		{"configuration", "version_validation", "policy_id", "publication rejected: category=version_validation field=policy_id [private error text omitted]", func(request *tracker.PublishChangeVersion) {
			request.PolicyID = "sentinel-secret-customer"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ChangeVersionInput: changeTestInput()}
			test.edit(&request)
			_, err := publisher.PublishChangeVersion(t.Context(), issue.WorkItemID, change.ID, request)
			var failure *hubclient.APIError
			if !errors.As(err, &failure) || failure.Status != http.StatusUnprocessableEntity || failure.Rejection == nil || failure.Rejection.Category != test.category || failure.Rejection.Field != test.field {
				t.Fatalf("version rejection=%+v error=%v", failure, err)
			}
			if strings.Contains(err.Error(), "sentinel-secret-customer") {
				t.Fatal("version error disclosed private request")
			}
			view.Diagnostics.Records[0].HostOperation = "native.publishVersion"
			view.Diagnostics.Records[0].LatestError = failure.Rejection.DiagnosticError()
			post()
			page, err := call(ctx, operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID, AttemptID: event.Data.AttemptID})
			if err != nil || len(page.Records) != 1 || page.Records[0].LatestError != test.want || page.Records[0].Unavailable["publication_rejection"] != "" || page.Records[0].NativeAttemptID != event.Data.AttemptID || !page.Records[0].OperationObservedAt.Equal(now) || !page.Records[0].OperationPending {
				t.Fatalf("version diagnostic=%+v error=%v", page, err)
			}
		})
	}
	var versions, published int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_versions WHERE change_id=?", change.ID).Scan(&versions); err != nil || versions != 0 {
		t.Fatalf("rejected version was accepted: %d %v", versions, err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='change.version_published'", issue.WorkItemID).Scan(&published); err != nil || published != 0 {
		t.Fatalf("rejected version fabricated history: %d %v", published, err)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, changePath+"/"+change.ID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, response, &detail)
	if detail.Change.CurrentVersion != "" || detail.Change.Revision != change.Revision {
		t.Fatalf("rejection changed current version: %+v", detail.Change)
	}
	view.Diagnostics.Records[0] = original
	post()
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

func TestRunnerProjectDiagnosticsIsolationPlatform(t *testing.T) {
	for _, test := range []struct {
		name, goos, tier string
		unsupported      bool
		misreported      bool
	}{
		{"windows sandbox", "windows", isolation.Sandbox, true, false},
		{"windows sandbox misreported", "windows", isolation.Sandbox, true, true},
		{"windows full access", "windows", isolation.NativeTrusted, false, false},
		{"darwin sandbox", "darwin", isolation.Sandbox, false, false},
		{"linux sandbox", "linux", isolation.Sandbox, false, false},
		{"legacy unspecified platform", "", isolation.Sandbox, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.redemption.IsolationTier = test.tier
			r.redemption.OS = test.goos
			if test.goos == "windows" {
				r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
			}
			if test.misreported {
				r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}}
			}
			r.enroll(t)
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID, r.redemption.Credential, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page runnerauth.DiagnosticPage
			decodeHubResponse(t, response, &page)
			reason := page.Unavailable["isolation_tier"]
			if test.unsupported {
				if !strings.Contains(reason, "sandbox is unavailable on this platform") || !strings.Contains(reason, "native-trusted") {
					t.Fatalf("diagnostics omitted platform reason: %+v", page)
				}
			} else if reason != "" {
				t.Fatalf("supported platform tier reported unavailable: %+v", page)
			}
			issue := f.create(t, "queued")
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			want := http.StatusOK
			if test.unsupported {
				want = http.StatusConflict
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), want)
		})
	}
}
