package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestRunnerProjectDiagnosticOwner(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
	f := newDefaultNativeFixture(t, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
	r.enroll(t)
	now := f.service.config.Clock()
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
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type LIKE 'run.%'").Scan(&accepted); err != nil || accepted != 0 {
		t.Fatalf("rejected event fabricated history: %d %v", accepted, err)
	}

	view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Records: []runnerauth.DiagnosticAttempt{{Key: "local:5", IssueID: "wi_unmatched", LocalAttemptID: 5, Membership: []string{"durable_active"}, NativeAttemptID: event.Data.AttemptID, HostOperation: "append.run.observed", OperationPending: true, OperationObservedAt: now, LatestErrorCode: "invalid_request", LatestError: rejection.Rejection.DiagnosticError(), Unavailable: map[string]string{"native_attempt_id": "unrecorded"}}}, Admissions: []runnerauth.DiagnosticAdmission{{IssueID: "wi_ready", ObservedAt: now, Result: "skipped", Predicate: "project_capacity_full", Unavailable: map[string]string{}}}, Counts: map[string]int{"durable_active": 1}, Unavailable: map[string]string{"running_build": "unavailable"}}}
	post := func() {
		t.Helper()
		clock.Add(int64(time.Second))
		if view.Diagnostics != nil {
			view.Diagnostics.ObservedAt = view.Diagnostics.ObservedAt.Add(time.Second)
		}
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
	storedState, stored := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
	if stored.Diagnostics == nil || len(stored.Diagnostics.Records) != 1 {
		t.Fatalf("stored diagnostics retained historical records: %+v", stored)
	}
	t.Run("expires after preparation", func(t *testing.T) {
		preparedAt := f.service.config.Clock()
		view.Diagnostics.ObservedAt = view.Diagnostics.ObservedAt.Add(time.Second)
		view.Diagnostics.Records[1].LeaseExpiresAt = preparedAt.Add(time.Second)
		defer func() { view.Diagnostics.Records[1].LeaseExpiresAt = now.Add(-time.Hour) }()
		view.Paused = true
		var prepared, advanced atomic.Bool
		timed := func(ctx context.Context) context.Context {
			return postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, query string) {
				if strings.HasPrefix(query, "SELECT request_id,request_operation,") {
					prepared.Store(true)
				}
				if query == "COMMIT" && prepared.Load() && advanced.CompareAndSwap(false, true) {
					clock.Store(preparedAt.Add(3 * time.Second).UnixNano())
				}
			})
		}
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request.WithContext(timed(request.Context())))
		requireNativeStatus(t, response, http.StatusOK)
		if !advanced.Load() {
			t.Fatal("clock did not advance after preparation")
		}
		state, snapshot := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
		if snapshot.Diagnostics == nil || len(snapshot.Diagnostics.Records) != 1 || snapshot.Diagnostics.Records[0].Key != "local:5" {
			t.Fatalf("stored diagnostics retained expired record: %+v", snapshot)
		}
		if !snapshot.Paused {
			t.Fatal("stored configuration lost simultaneous pause change")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID+"&issue_id=wi_unmatched", r.redemption.Credential, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var projected runnerauth.DiagnosticPage
		decodeHubResponse(t, response, &projected)
		if len(projected.Records) != 1 || projected.Records[0].Key != "local:5" {
			t.Fatalf("typed diagnostics disagree with stored snapshot: %+v", projected)
		}
		if len(state.Attempts) != 1 || state.Attempts[0].RecordKey != "local:5" {
			t.Fatalf("typed diagnostics retained expired records: %+v", state.Attempts)
		}
		digest := state.Observation.SnapshotDigest
		wantDigest, err := projectConfigurationDigest(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if digest != wantDigest {
			t.Fatalf("stored digest = %s, want pruned snapshot digest %s", digest, wantDigest)
		}
	})
	t.Run("same-time conflicting report keeps the stored observation", func(t *testing.T) {
		other := newNativeFixture(t, f.service, f.project.OrganizationID+"_second", "second diagnostics")
		otherRunner := prepareRunner(t, other, runnerauth.Read, runnerauth.Heartbeat)
		otherRunner.enroll(t)
		heartbeat := func(runner runnerFixture, base string, report runnerauth.ProjectConfiguration) *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodPost, base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": runner.redemption.BackendIsolation, "project_configuration": report})
		}
		clone := func(report runnerauth.ProjectConfiguration) runnerauth.ProjectConfiguration {
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var copied runnerauth.ProjectConfiguration
			if err := json.Unmarshal(raw, &copied); err != nil {
				t.Fatal(err)
			}
			return copied
		}
		otherView := clone(view)
		otherView.ProjectID = string(other.project.ID)
		requireNativeStatus(t, heartbeat(otherRunner, other.base, otherView), http.StatusOK)
		beforeState, before := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
		otherBefore, _ := readDiagnosticFixtureState(t, other, otherRunner.binding.RunnerID)
		conflicting := clone(view)
		conflicting.Diagnostics.Admissions[0].Predicate = "conflicting_same_time"
		clock.Add(int64(time.Second))
		otherView.Diagnostics.ObservedAt = otherView.Diagnostics.ObservedAt.Add(time.Second)
		responses := make(chan *httptest.ResponseRecorder, 3)
		var reports sync.WaitGroup
		for range 2 {
			reports.Go(func() { responses <- heartbeat(r, f.base, conflicting) })
		}
		reports.Go(func() { responses <- heartbeat(otherRunner, other.base, otherView) })
		reports.Wait()
		close(responses)
		conflicts := 0
		for response := range responses {
			if response.Code == http.StatusConflict && strings.Contains(response.Body.String(), `"code":"revision_conflict"`) {
				conflicts++
				continue
			}
			requireNativeStatus(t, response, http.StatusOK)
		}
		if conflicts != 2 {
			t.Fatalf("same-time conflicting reports answered %d typed conflicts, want 2", conflicts)
		}
		afterState, after := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
		if !reflect.DeepEqual(afterState.Observation, beforeState.Observation) || !reflect.DeepEqual(afterState.Attempts, beforeState.Attempts) || afterState.Command.Observation != beforeState.Command.Observation || !reflect.DeepEqual(after.Diagnostics, before.Diagnostics) {
			t.Fatalf("conflicting report changed the stored observation: before=%+v after=%+v", beforeState.Observation, afterState.Observation)
		}
		if otherAfter, _ := readDiagnosticFixtureState(t, other, otherRunner.binding.RunnerID); otherAfter.Observation.DiagnosticsAt == nil || otherBefore.Observation.DiagnosticsAt == nil || !otherAfter.Observation.DiagnosticsAt.After(*otherBefore.Observation.DiagnosticsAt) || !otherAfter.Observation.ObservedAt.After(otherBefore.Observation.ObservedAt) {
			t.Fatalf("other organization's newer report was not stored: before=%+v after=%+v", otherBefore.Observation, otherAfter.Observation)
		}
		clock.Add(int64(time.Second))
		requireNativeStatus(t, heartbeat(r, f.base, view), http.StatusOK)
		if replayState, _ := readDiagnosticFixtureState(t, f, r.binding.RunnerID); replayState.Observation.SnapshotDigest != beforeState.Observation.SnapshotDigest {
			t.Fatalf("identical replay changed the snapshot digest: before=%s after=%s", beforeState.Observation.SnapshotDigest, replayState.Observation.SnapshotDigest)
		}
	})
	for _, runtimeFresh := range []bool{true, true, false} {
		view.ObservedAt = now.Add(-2 * runnerauth.HeartbeatTimeout)
		view.Diagnostics.ObservedAt = view.Diagnostics.ObservedAt.Add(time.Second)
		view.Diagnostics.RuntimeObservedAt = time.Time{}
		if runtimeFresh {
			view.Diagnostics.RuntimeObservedAt = view.Diagnostics.ObservedAt
		}
		clock.Add(int64(time.Second))
		received := f.service.config.Clock()
		beforeState, _ := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
		var traceMu sync.Mutex
		statements, payloadUpdates := 0, 0
		timed := func(ctx context.Context) context.Context {
			return postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, query string) {
				traceMu.Lock()
				defer traceMu.Unlock()
				statements++
				query = strings.ToUpper(strings.TrimSpace(query))
				for strings.HasPrefix(query, "--") {
					_, query, _ = strings.Cut(query, "\n")
					query = strings.TrimSpace(query)
				}
				if strings.HasPrefix(query, "UPDATE RUNNER_PROJECT_CONFIGURATIONS") && strings.Contains(strings.SplitN(query, "WHERE", 2)[0], "PAYLOAD_OBJECT_KEY=") {
					payloadUpdates++
				}
			})
		}
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request.WithContext(timed(request.Context())))
		requireNativeStatus(t, response, http.StatusOK)
		traceMu.Lock()
		observedStatements, observedPayloadUpdates := statements, payloadUpdates
		traceMu.Unlock()
		if observedStatements == 0 || observedPayloadUpdates != 0 {
			t.Fatalf("configuration trace: statements=%d, payload updates=%d, want observed statements and 0 payload updates", observedStatements, observedPayloadUpdates)
		}
		storedState, stored = readDiagnosticFixtureState(t, f, r.binding.RunnerID)
		if storedState.Configuration.PayloadObjectKey != beforeState.Configuration.PayloadObjectKey || storedState.Configuration.PayloadSHA256 != beforeState.Configuration.PayloadSHA256 || storedState.Command.Observation != beforeState.Command.Observation {
			t.Fatal("telemetry-only heartbeat rewrote configuration payload")
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
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(second.binding.MachineID)+"/heartbeat", second.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": second.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
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
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM change_versions WHERE change_id=$1", change.ID).Scan(&versions); err != nil || versions != 0 {
		t.Fatalf("rejected version was accepted: %d %v", versions, err)
	}
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=$1 AND type='change.version_published'", issue.WorkItemID).Scan(&published); err != nil || published != 0 {
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
	before, _ := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
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
	after, _ := readDiagnosticFixtureState(t, f, r.binding.RunnerID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read mutated runner configuration, identity or diagnostics")
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
	clock.Add(int64(runnerauth.HeartbeatTimeout + time.Second))
	post()
	page, err := call(ctx, args)
	if err != nil || page.Status != "stale" {
		t.Fatalf("stale=%+v %v", page, err)
	}
	view.Diagnostics = nil
	post()
	page, err = call(ctx, args)
	if err != nil || page.Status != "stale" || len(page.Admissions) != 1 {
		t.Fatalf("report without diagnostics replaced the stored snapshot=%+v %v", page, err)
	}
	older := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	older.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(older.binding.MachineID)+"/heartbeat", older.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "rolling.current", "protocol_major": 2, "backend_isolation": older.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
	olderArgs := args
	olderArgs.RunnerID = older.binding.RunnerID
	page, err = call(ctx, olderArgs)
	if err != nil || page.Status != "unavailable" || page.Unavailable["snapshot"] == "" {
		t.Fatalf("older=%+v %v", page, err)
	}
	if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = $1 WHERE runner_id = $2 AND organization_id=$3", now.Add(-runnerauth.HeartbeatTimeout-time.Minute), r.binding.RunnerID, f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	page, err = call(ctx, args)
	if err != nil || page.Status != "offline" || page.RunnerID != r.binding.RunnerID {
		t.Fatalf("offline=%+v %v", page, err)
	}
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-diagnostics")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign.base+"/runner-diagnostics", foreign.token, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign.base+"/runner-diagnostics", r.redemption.Credential, nil), http.StatusNotFound)
	if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = $1 WHERE id = (SELECT token_id FROM runner_identities WHERE runner_id=$2 AND organization_id=$3)", now, r.binding.RunnerID, f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/runner-diagnostics?runner_id="+r.binding.RunnerID, r.redemption.Credential, nil), http.StatusUnauthorized)
	if strings.Contains(logs.String(), "sentinel-secret-customer") {
		t.Fatal("request secret leaked into logs")
	}
}

type diagnosticFixtureState struct {
	Identity      runnerstore.Identity
	Configuration postgres.ConfigurationProject
	Command       postgres.ConfigurationCommand
	Observation   runnerstore.ProjectObservation
	Attempts      []runnerstore.ProjectDiagnosticAttempt
}

func readDiagnosticFixtureState(t *testing.T, f nativeFixture, runnerID string) (diagnosticFixtureState, runnerauth.ProjectConfiguration) {
	t.Helper()
	var state diagnosticFixtureState
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		state.Identity, err = reader.RunnerIdentity(t.Context(), runnerID)
		if err != nil {
			return err
		}
		owner := reader.(postgres.ConfigurationReader)
		state.Configuration, err = owner.ConfigurationProject(t.Context(), runnerID, f.project.ID)
		if err != nil {
			return err
		}
		state.Command, err = owner.ConfigurationCommand(t.Context(), runnerID, f.project.ID)
		if err != nil {
			return err
		}
		state.Observation, state.Attempts, err = reader.RunnerProjectObservation(t.Context(), runnerID, f.project.ID, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var reported runnerauth.ProjectConfiguration
	if state.Command.Observation.Key == "" {
		t.Fatal("runner configuration observation payload missing")
	}
	if err := f.service.readConfigurationPayload(t.Context(), f.project.OrganizationID, state.Command.Observation, &reported); err != nil {
		t.Fatal(err)
	}
	reported.ObservedAt = state.Observation.ObservedAt
	reported.Diagnostics = state.Observation.Diagnostics
	return state, reported
}
