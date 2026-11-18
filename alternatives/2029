package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestHostedEventsWorkspace(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.Workspace = &WorkspaceConfig{Enabled: true}
	})
	user := f.user(t, "member", "member", "member@example.test", "write", "")
	item := f.seedIssue(t, 1)
	before := dashboardPostgresHead(t, f)
	response := f.request(t, user, http.MethodPost, f.base+"/workspaces", workspaceRequest{
		Mutation: tracker.Mutation{IdempotencyKey: "stream-workspace"}, WorkItemID: string(item), Requires: []string{"files"},
	})
	requireNativeStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	scanner := openHostedTestStream(t, f, user, "?workspace="+session.ID+"&since="+strconv.FormatInt(before, 10))
	for _, state := range []string{"requested", "starting", "ready", "idle", "closed"} {
		if !t.Run(state, func(t *testing.T) {
			if state == workspacesession.StateClosed {
				requireNativeStatus(t, f.request(t, user, http.MethodDelete, f.base+"/workspaces/"+session.ID, nil), http.StatusNoContent)
			} else if state != workspacesession.StateRequested {
				err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(writer tenantstore.Writer) error {
					owner := writer.(postgres.WorkspaceWriter)
					record, err := owner.WorkspaceSession(t.Context(), f.project, session.ID)
					if err != nil {
						return err
					}
					expected := record
					record.Capabilities = []string{"files"}
					record.ReadOnly = true
					now := f.service.config.Clock()
					if state == workspacesession.StateReady && record.OpenedAt == nil {
						record.OpenedAt = &now
					}
					_, err = updateWorkspaceState(t.Context(), owner, record, expected, state, "", now)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			for {
				typeName, data := readHostedEvent(t, scanner)
				if !strings.HasPrefix(typeName, "workspace.") {
					continue
				}
				if state == workspacesession.StateClosed && typeName == "workspace.closing" {
					continue
				}
				if typeName != workspacesession.EventType(state) {
					t.Fatalf("event = %q, want workspace.%s", typeName, state)
				}
				var actual workspacesession.Session
				if err := json.Unmarshal([]byte(data), &actual); err != nil {
					t.Fatal(err)
				}
				api := f.request(t, user, http.MethodGet, f.base+"/workspaces/"+session.ID, nil)
				requireNativeStatus(t, api, http.StatusOK)
				var expected workspacesession.Session
				decodeHubResponse(t, api, &expected)
				if !reflect.DeepEqual(actual, expected) || actual.ID != session.ID || actual.State != state || actual.RelaySessions != nil {
					t.Fatalf("event resource = %#v, API = %#v", actual, expected)
				}
				break
			}
			if state == workspacesession.StateReady {
				response := f.request(t, user, http.MethodPost, f.base+"/actions", projectActionRequest{Mutation: tracker.Mutation{IdempotencyKey: "stream-action"}, Name: "Example action", Command: "true"})
				requireNativeStatus(t, response, http.StatusCreated)
				var action workspacesession.Action
				decodeHubResponse(t, response, &action)
				runID := hubapi.NewID("run")
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.(postgres.RelayActionWriter).CreateActionRun(t.Context(), postgres.CreateActionRunInput{ID: runID, OrganizationID: f.service.accessOrganization(), ProjectID: f.project, ActionID: action.ID, WorkspaceID: session.ID, CreatedBy: user.identity.Subject, CreatedAt: f.service.config.Clock()})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for {
					kind, data := readHostedEvent(t, scanner)
					if !strings.HasPrefix(kind, "action_run.") {
						continue
					}
					var actual workspacesession.Run
					if err := json.Unmarshal([]byte(data), &actual); err != nil {
						t.Fatal(err)
					}
					response := f.request(t, user, http.MethodGet, f.base+"/actions/"+action.ID+"/runs/"+runID, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var expected workspacesession.Run
					decodeHubResponse(t, response, &expected)
					if kind != "action_run.queued" || actual.ID != runID || actual.WorkspaceID != session.ID || actual.Command != "true" || !reflect.DeepEqual(actual, expected) {
						t.Fatalf("action event=%q resource=%#v API=%#v", kind, actual, expected)
					}
					break
				}
			}
		}) {
			return
		}
	}
	reconnect := openHostedTestStream(t, f, user, "?workspace="+session.ID+"&since="+strconv.FormatInt(before, 10))
	var kind, data string
	for kind != "workspace.closed" {
		kind, data = readHostedEvent(t, reconnect)
	}
	var resumed workspacesession.Session
	if err := json.Unmarshal([]byte(data), &resumed); err != nil || kind != "workspace.closed" || resumed.State != "closed" {
		t.Fatalf("reconnected resource = %q %#v: %v", kind, resumed, err)
	}
	var open int
	err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(reader tenantstore.Reader) error {
		item, err := reader.(postgres.WorkspaceReader).WorkspaceDispatchItem(t.Context(), f.project, session.ID)
		if err != nil {
			return err
		}
		if item.ClosedAt == nil {
			open++
		}
		return nil
	})
	if err != nil || open != 0 {
		t.Fatalf("dispatch association still open = %d: %v", open, err)
	}
	requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/workspaces", workspaceRequest{
		Mutation: tracker.Mutation{IdempotencyKey: "terminal-disabled"}, WorkItemID: string(item), Requires: []string{"terminal"},
	}), http.StatusForbidden)
	other := f
	other.project = "prj_other"
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(writer tenantstore.Writer) error {
		_, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: other.project, Name: "Other project", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	other.grant(t, user, false, false)
	for _, test := range []struct {
		name      string
		project   string
		workspace string
		status    int
	}{
		{"invalid workspace", string(f.project), "invalid", http.StatusNotFound},
		{"missing workspace", string(f.project), workspacesession.NewID(), http.StatusNotFound},
		{"ungranted project", "prj_ungranted", session.ID, http.StatusNotFound},
		{"workspace from another project", string(other.project), session.ID, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+test.project+"/events?workspace="+test.workspace, nil), test.status)
		})
	}
}

func TestHostedEventsActivityIncludesChangesBelowIssueMaximum(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	high, low := f.seedIssue(t, 1), f.seedIssue(t, 2)
	scope := hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: hubapi.APICredential{ID: "fixture", Scope: hubapi.APIScopeAdmin}}
	appendEvent := func(item tracker.NativeWorkItemID, kind string, data tracker.CollaborationData) {
		t.Helper()
		appendAnalyticsTestEvent(t, f.service, scope, item, kind, data, f.service.config.Clock())
	}
	for range 10 {
		appendEvent(high, "issue.edited", tracker.CollaborationData{})
	}
	var previous int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		watermark, err := r.BoardSequence(t.Context(), scope.Project)
		previous = watermark.Sequence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	scanner := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(previous, 10))
	appendEvent(low, "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{Outcome: "interrupted"}})
	kind, data := readHostedActivity(t, scanner)
	next, err := strconv.ParseInt(data, 10, 64)
	if kind != "activity" || err != nil || next <= previous {
		t.Fatalf("lower issue activity=%q %q, previous=%d: %v", kind, data, previous, err)
	}

}

func TestHostedEventsHealthFinding(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "member", "member", "member@example.test", "read", "")
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	finding := newHealthFinding("dead_man", "flow", "project", string(f.project), "No landings", "Check admission", []string{string(f.project)}, healthEvidence{})
	scope := hubscope.NativeScope{Organization: "org_security", Project: f.project}
	write := func(at time.Time, findings []healthFinding) int64 {
		t.Helper()
		var previous int64
		err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			watermark, err := r.BoardSequence(t.Context(), scope.Project)
			previous = watermark.Sequence
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.publishHealthEvaluation(t.Context(), scope.Organization, at, findings); err != nil {
			t.Fatal(err)
		}
		return previous
	}
	previous := write(now, []healthFinding{finding})
	scanner := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(previous, 10))
	var findingID string
	for _, state := range []struct {
		at     time.Time
		status string
	}{
		{now, "attention"},
		{now.Add(time.Minute), "resolved"},
	} {
		if state.at != now {
			previous = write(state.at, nil)
		}
		for {
			kind, data := readHostedEvent(t, scanner)
			if kind != "health.findings" {
				continue
			}
			var change dashboardChange
			if err := json.Unmarshal([]byte(data), &change); err != nil || change.Topic != "health" {
				t.Fatalf("finding frame = %q %q: %v", kind, data, err)
			}
			if change.Sequence <= previous {
				continue
			}
			if change.Status != state.status || change.EntityID == "" || (findingID != "" && change.EntityID != findingID) {
				t.Fatalf("finding change = %+v, want %s for %q", change, state.status, findingID)
			}
			findingID = change.EntityID
			break
		}
	}
	page, err := f.service.readHealthFindings(t.Context(), hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: hubapi.APICredential{Scope: hubapi.APIScopeAdmin}}, "open", "", "", 100)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("resolved findings = %#v: %v", page, err)
	}
}

func TestHostedEventsIssue(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	item, unrelated := f.seedIssue(t, 1), f.seedIssue(t, 2)
	scope := hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: hubapi.APICredential{ID: "fixture", Scope: hubapi.APIScopeAdmin}}
	var previous int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		watermark, err := r.BoardSequence(t.Context(), scope.Project)
		previous = watermark.Sequence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	scanner := openHostedTestStream(t, f, user, "?work_item="+string(item)+"&since="+strconv.FormatInt(previous, 10))

	for _, test := range []struct {
		name    string
		item    tracker.NativeWorkItemID
		kind    string
		changed bool
	}{
		{"unrelated completion", unrelated, "run.finished", false},
		{"selected completion", item, "run.finished", true},
		{"selected change", item, "change.version_published", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			appendAnalyticsTestEvent(t, f.service, scope, test.item, test.kind, tracker.CollaborationData{}, f.service.config.Clock())
			next := previous
			for {
				kind, data := readHostedEvent(t, scanner)
				if kind == "checkpoint" && !test.changed {
					break
				}
				if kind != "activity" {
					continue
				}
				value, err := strconv.ParseInt(data, 10, 64)
				if kind != "activity" || err != nil {
					t.Fatalf("activity=%q %q err=%v", kind, data, err)
				}
				next = value
				if next > previous {
					break
				}
			}
			if (next > previous) != test.changed {
				t.Fatalf("activity sequence %d -> %d, changed want %v", previous, next, test.changed)
			}
			previous = next
		})
	}
	for _, query := range []string{"?work_item=invalid", "?work_item=wi_missing"} {
		requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events"+query, nil), http.StatusNotFound)
	}
	config, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	config.ConnConfig.Tracer = runtimeHistoryTrace{}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	capture := &runtimeHistoryTraceCapture{}
	ctx := context.WithValue(t.Context(), runtimeHistoryTraceKey{}, capture)
	if err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(ctx, scope.Project, item)
		if err == nil && (row.NativeID != item || row.EventSequence < 1) {
			return errors.New("selected issue observation did not return current evidence")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	statements := append([]tracedStatement(nil), capture.statements...)
	capture.mu.Unlock()
	var selected []tracedStatement
	for _, statement := range statements {
		if strings.HasPrefix(statement.query, "-- name: GetWorkItem :one") {
			selected = append(selected, statement)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("selected issue observation statements=%d, want 1", len(selected))
	}
	admin := hostedPostgresAdmin(t, f.service)
	if _, err := admin.ExecContext(t.Context(), "ANALYZE issues"); err != nil {
		t.Fatal(err)
	}
	plan, raw := reportsExplainPostgresPlan(t, admin, scope.Organization, selected[0], "enable_seqscan=off", "enable_bitmapscan=off")
	indexed, observed := false, float64(0)
	var inspect func(reportsBudgetPlan)
	inspect = func(node reportsBudgetPlan) {
		if node.Loops > 0 && node.Index != "" && strings.Contains(node.Condition, "native_id") {
			indexed = true
		}
		if node.Relation == "issues" {
			observed += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plan)
	if !indexed || observed != 1 {
		t.Fatalf("unbounded issue observation query: %s", raw)
	}
	t.Logf("selected issue observation query plan: %s", raw)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, scope.Project)
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events?work_item="+string(item), nil), http.StatusNotFound)
}

type hostedStreamWriter struct {
	*httptest.ResponseRecorder
	output  *io.PipeWriter
	headers chan *http.Response
}

func (w hostedStreamWriter) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	w.headers <- w.Result()
}

func (w hostedStreamWriter) Write(data []byte) (int, error) {
	return w.output.Write(data)
}

func openHostedTestStream(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser, query string) *bufio.Reader {
	t.Helper()
	return openHostedTestCursorStream(t, f, user, query, "")
}

func openHostedTestCursorStream(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser, query, last string) *bufio.Reader {
	t.Helper()
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	request := httptest.NewRequest(http.MethodGet, "/projects/"+string(f.project)+"/events"+query, nil).WithContext(ctx)
	if last != "" {
		request.Header.Set("Last-Event-ID", last)
	}
	request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
	input, output := io.Pipe()
	done := make(chan struct{})
	headers := make(chan *http.Response, 1)
	go func() {
		defer close(done)
		defer output.Close()
		f.service.Handler().ServeHTTP(hostedStreamWriter{ResponseRecorder: httptest.NewRecorder(), output: output, headers: headers}, request)
	}()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-ctx.Done()
		input.CloseWithError(ctx.Err())
	}()
	t.Cleanup(func() {
		cancel()
		input.Close()
		<-done
		<-watcherDone
	})
	select {
	case response := <-headers:
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream response = %d %s", response.StatusCode, response.Header.Get("Content-Type"))
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return bufio.NewReader(input)
}

func readHostedEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	frame, err := readSSEFrame(reader)
	if err != nil {
		t.Fatalf("read hosted frame: %v", err)
	}
	return frame.Event, frame.Data
}

func readHostedActivity(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	for {
		kind, data := readHostedEvent(t, reader)
		if kind == "activity" {
			return kind, data
		}
	}
}

func TestHostedEventsCursorWindow(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "cursor-viewer", "viewer", "cursor@example.test", "read", "")
	stream := openHostedTestStream(t, f, user, "?since=9223372036854775807")
	frame, err := readSSEFrame(stream)
	if err != nil || frame.Event != "reset" || frame.ID == "" || frame.Data != "{}" {
		t.Fatalf("out-of-window cursor: frame=%+v err=%v", frame, err)
	}
	if _, err = readSSEFrame(stream); !errors.Is(err, io.EOF) {
		t.Fatalf("reset must close: %v", err)
	}
}
