package hubserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestHostedWorkspaceEvents(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.Workspace = &WorkspaceConfig{Enabled: true}
	})
	user := f.user(t, "member", "member", "member@example.test", "write", "")
	item := f.seedIssue(t, 1)
	response := f.request(t, user, http.MethodPost, f.base+"/workspaces", workspaceRequest{
		Mutation: tracker.Mutation{IdempotencyKey: "stream-workspace"}, WorkItemID: string(item), Requires: []string{"files"},
	})
	requireNativeStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	scanner := openHostedTestStream(t, f, user, "?workspace="+session.ID)
	for _, state := range []string{"requested", "starting", "ready", "idle", "closed"} {
		if !t.Run(state, func(t *testing.T) {
			if state == workspacesession.StateClosed {
				requireNativeStatus(t, f.request(t, user, http.MethodDelete, f.base+"/workspaces/"+session.ID, nil), http.StatusNoContent)
			} else if state != workspacesession.StateRequested {
				err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, now time.Time) error {
					record, err := readWorkspaceByID(t.Context(), tx, session.ID)
					if err != nil {
						return err
					}
					record.Capabilities = &workspacesession.Capabilities{Files: true}
					record.ReadOnly = true
					_, err = f.service.workspaces.transition(t.Context(), tx, record, state, "", now)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			for {
				typeName, data := readHostedEvent(t, scanner)
				if typeName == "activity" {
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
		}) {
			return
		}
	}
	reconnect := openHostedTestStream(t, f, user, "?workspace="+session.ID)
	readHostedEvent(t, reconnect)
	kind, data := readHostedEvent(t, reconnect)
	var resumed workspacesession.Session
	if err := json.Unmarshal([]byte(data), &resumed); err != nil || kind != "workspace.closed" || resumed.State != "closed" {
		t.Fatalf("reconnected resource = %q %#v: %v", kind, resumed, err)
	}
	var open int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM workspace_items WHERE workspace_id = ? AND closed_at IS NULL", session.ID).Scan(&open); err != nil || open != 0 {
		t.Fatalf("dispatch association still open = %d: %v", open, err)
	}
	requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/workspaces", workspaceRequest{
		Mutation: tracker.Mutation{IdempotencyKey: "terminal-disabled"}, WorkItemID: string(item), Requires: []string{"terminal"},
	}), http.StatusForbidden)
	other := f
	other.project = "prj_other"
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO projects(id, organization_id, name, profile, states_json, created_at, github_repository_enabled)
VALUES (?, 'org_security', 'Other project', 'native', '[]', ?, 0)`, other.project, formatHubTime(time.Now())); err != nil {
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
		{"ungranted project", "prj_ungranted", session.ID, http.StatusForbidden},
		{"workspace from another project", string(other.project), session.ID, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+test.project+"/events?workspace="+test.workspace, nil), test.status)
		})
	}
}

func TestHostedActivityIncludesChangesBelowIssueMaximum(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	high, low := f.seedIssue(t, 1), f.seedIssue(t, 2)
	scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: "fixture", Scope: apiScopeAdmin}}
	appendEvent := func(item tracker.NativeWorkItemID, kind string, data tracker.CollaborationData) {
		t.Helper()
		if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, now time.Time) error {
			return appendNativeHistory(t.Context(), tx, scope, string(item), kind, data, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		appendEvent(high, "issue.edited", tracker.CollaborationData{})
	}
	scanner := openHostedTestStream(t, f, user, "")
	kind, data := readHostedEvent(t, scanner)
	previous, err := strconv.ParseInt(data, 10, 64)
	if kind != "activity" || err != nil {
		t.Fatalf("initial activity = %q %q: %v", kind, data, err)
	}
	appendEvent(low, "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{Outcome: "interrupted"}})
	for {
		kind, data = readHostedEvent(t, scanner)
		next, err := strconv.ParseInt(data, 10, 64)
		if kind != "activity" || err != nil {
			t.Fatalf("activity = %q %q: %v", kind, data, err)
		}
		if next > previous {
			break
		}
	}
}

func TestHostedIssueEvents(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	item, unrelated := f.seedIssue(t, 1), f.seedIssue(t, 2)
	scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: "fixture", Scope: apiScopeAdmin}}
	scanner := openHostedTestStream(t, f, user, "?work_item="+string(item))
	kind, data := readHostedEvent(t, scanner)
	previous, err := strconv.ParseInt(data, 10, 64)
	if kind != "activity" || err != nil {
		t.Fatalf("initial activity = %q %q: %v", kind, data, err)
	}
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
			if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, now time.Time) error {
				return appendNativeHistory(t.Context(), tx, scope, string(test.item), test.kind, tracker.CollaborationData{}, now)
			}); err != nil {
				t.Fatal(err)
			}
			kind, data := readHostedEvent(t, scanner)
			next, err := strconv.ParseInt(data, 10, 64)
			if kind != "activity" || err != nil || (next > previous) != test.changed {
				t.Fatalf("activity=%q %q changed=%v err=%v", kind, data, test.changed, err)
			}
			previous = next
		})
	}
	for _, query := range []string{"?work_item=invalid", "?work_item=wi_missing"} {
		requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events"+query, nil), http.StatusNotFound)
	}
	rows, err := f.service.database.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT event_sequence FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", scope.organization, scope.project, item)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		if !strings.Contains(detail, "SEARCH issues USING INDEX") {
			t.Fatalf("unbounded issue observation query: %s", detail)
		}
	}
	if err := rows.Err(); err != nil || len(plan) == 0 {
		t.Fatalf("query plan=%v err=%v", plan, err)
	}
	t.Logf("selected issue observation query plan: %v", plan)
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id = ?", user.identity.Subject); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events?work_item="+string(item), nil), http.StatusForbidden)
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	request := httptest.NewRequest(http.MethodGet, "/projects/"+string(f.project)+"/events"+query, nil).WithContext(ctx)
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
