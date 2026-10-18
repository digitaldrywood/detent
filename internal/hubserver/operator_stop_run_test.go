package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubMCPStopRun(t *testing.T) {
	for _, scenario := range []string{"execute", "stale revision", "stale fence", "replacement", "foreign project", "foreign item", "missing revision", "missing lease", "missing fence", "release rollback", "routing rollback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			version := f.publish(t, "source", "", true)
			sourcePath := f.path + "/versions/" + version.ID + "/source"
			source := performHubAPIRequest(t, f.service, http.MethodGet, sourcePath, f.token, nil)
			requireNativeStatus(t, source, http.StatusOK)
			unrelated := f.create(t, "unrelated")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f.nativeFixture, worker, "machine", "session", f.issue.WorkItemID)
			otherLease := claimNativeAttempt(t, f.nativeFixture, worker, "other-machine", "other-session", unrelated.WorkItemID)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE native_id=?", f.project.ID, f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			start := nativeStartedEvent(lease)
			checkpoint := start
			checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence, checkpoint.Data.Handoff = "run.checkpointed", "checkpoint", 2, nativeTestCheckpoint()
			finish := start
			finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
			for _, event := range []tracker.NativeRunEvent{start, checkpoint, finish} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, event), http.StatusOK)
			}
			issue := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
			unrelated = readWorkItem(t, f.nativeFixture, unrelated.WorkItemID, "")
			query := f.service.database.reader
			readLease := func(id tracker.LeaseID) (leaseRecord, bool, error) {
				t.Helper()
				tx, err := query.BeginTx(t.Context(), nil)
				if err != nil {
					return leaseRecord{}, false, err
				}
				record, found, err := readLeaseByID(t.Context(), tx, id)
				if rollbackErr := tx.Rollback(); rollbackErr != nil {
					t.Fatal(rollbackErr)
				}
				return record, found, err
			}
			active, found, err := readLease(lease.ID)
			if err != nil || !found || active.session.ReleasedAt != nil {
				t.Fatalf("completed attempt lost active lease: %+v %v", active, err)
			}
			otherBefore, found, err := readLease(otherLease.ID)
			if err != nil || !found {
				t.Fatalf("missing unrelated lease: %v", err)
			}
			fields := map[string]any{"project_id": f.project.ID, "request_id": "stop", "identifier": issue.WorkItemID, "expected_revision": fmt.Sprint(issue.Revision), "lease_id": lease.ID, "fencing_token": lease.FencingToken, "destination": "Todo", "priority": 2, "reason": "Release completed attempt"}
			db := f.service.database.db
			switch scenario {
			case "stale revision":
				fields["expected_revision"] = fmt.Sprint(issue.Revision + 1)
			case "stale fence":
				fields["fencing_token"] = lease.FencingToken + 1
			case "replacement":
				if _, err := db.ExecContext(t.Context(), "UPDATE leases SET released_at=updated_at WHERE lease_id=?", lease.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO leases(lease_id,issue_id,machine_id,session_id,fencing_token,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT 'replacement-lease',issue_id,machine_id,'replacement-session',(SELECT max(fencing_token)+1 FROM leases),expires_at,acquired_at,renewed_at,created_at,updated_at FROM leases WHERE lease_id=?", lease.ID); err != nil {
					t.Fatal(err)
				}
				lease.ID = "replacement-lease"
			case "foreign project":
				foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign")
				fields["project_id"] = foreign.project.ID
			case "foreign item":
				foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign")
				fields["identifier"] = foreign.create(t, "foreign").WorkItemID
			case "missing revision":
				delete(fields, "expected_revision")
			case "missing lease":
				delete(fields, "lease_id")
			case "missing fence":
				delete(fields, "fencing_token")
			case "release rollback":
				if _, err := db.ExecContext(t.Context(), "CREATE TRIGGER fail_stop_release BEFORE UPDATE OF released_at ON leases BEGIN SELECT RAISE(ABORT, 'release failed'); END"); err != nil {
					t.Fatal(err)
				}
			case "routing rollback":
				if _, err := db.ExecContext(t.Context(), "CREATE TRIGGER fail_stop_route BEFORE UPDATE OF workflow_state_id ON issues BEGIN SELECT RAISE(ABORT, 'routing failed'); END"); err != nil {
					t.Fatal(err)
				}
			}
			var sessionID string
			send := func(method string, params any) *httptest.ResponseRecorder {
				t.Helper()
				body := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
				if !strings.HasPrefix(method, "notifications/") {
					body["id"] = 1
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/mcp", bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+f.token)
				if sessionID != "" {
					req.Header.Set("Mcp-Session-Id", sessionID)
					req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, req)
				status := http.StatusOK
				if strings.HasPrefix(method, "notifications/") {
					status = http.StatusAccepted
				}
				requireNativeStatus(t, response, status)
				return response
			}
			initialized := send("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "stop-client", "version": "1"}})
			sessionID = initialized.Header().Get("Mcp-Session-Id")
			if sessionID == "" {
				t.Fatal("missing session")
			}
			send("notifications/initialized", map[string]any{})
			listed := send("tools/list", map[string]any{"_meta": map[string]any{"detent/toolsets": []string{"actions"}}})
			var catalog struct {
				Result struct {
					Tools []operatortool.Definition `json:"tools"`
				} `json:"result"`
			}
			decodeHubResponse(t, listed, &catalog)
			index := slices.IndexFunc(catalog.Result.Tools, func(d operatortool.Definition) bool { return d.Name == operatortool.StopRun })
			if index < 0 {
				t.Fatal("stop_run missing from authorized catalog")
			}
			var schema struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(catalog.Result.Tools[index].InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"request_id", "expected_revision", "lease_id", "fencing_token"} {
				if schema.Properties[field] == nil {
					t.Fatalf("catalog omits required %s", field)
				}
			}
			call := func(name string, arguments any) (json.RawMessage, bool) {
				t.Helper()
				response := send("tools/call", map[string]any{"name": name, "arguments": arguments})
				var envelope struct {
					Result struct {
						IsError bool `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &envelope)
				if len(envelope.Result.Content) != 1 {
					t.Fatalf("invalid response: %s", response.Body)
				}
				return json.RawMessage(envelope.Result.Content[0].Text), envelope.Result.IsError
			}
			raw, failed := call(operatortool.StopRun, fields)
			if failed != (scenario != "execute") {
				t.Fatalf("failed=%t: %s", failed, raw)
			}
			if scenario == "stale revision" && !strings.Contains(string(raw), "conflict") {
				t.Fatalf("revision refusal lost conflict: %s", raw)
			}
			if (scenario == "stale fence" || scenario == "replacement") && !strings.Contains(string(raw), "lease_conflict") {
				t.Fatalf("fence refusal lost conflict: %s", raw)
			}
			if scenario == "execute" {
				var result struct {
					Preview chat.Action       `json:"preview"`
					ID      string            `json:"action_id"`
					Status  chat.ActionStatus `json:"status"`
				}
				if err := json.Unmarshal(raw, &result); err != nil || result.ID == "" || result.Status != chat.ActionSucceeded {
					t.Fatalf("missing success receipt: %s %v", raw, err)
				}
				var receipt nativeStopRunReceipt
				if err := json.Unmarshal([]byte(result.Preview.Result), &receipt); err != nil || receipt.LeaseID != lease.ID || receipt.FencingToken != lease.FencingToken || receipt.Destination != "Todo" || receipt.WorkItemID != issue.WorkItemID || receipt.State != "Todo" || receipt.Revision != issue.Revision+1 {
					t.Fatalf("incomplete stop receipt: %+v %v", receipt, err)
				}
				polled, failed := call(operatortool.ActionResult, map[string]string{"action_id": result.ID})
				if failed || !bytes.Equal(raw, polled) {
					t.Fatalf("action_result changed receipt: %s", polled)
				}
				replay, failed := call(operatortool.StopRun, fields)
				if failed || !bytes.Equal(raw, replay) {
					t.Fatalf("replay changed receipt: %s", replay)
				}
				changed := maps.Clone(fields)
				changed["reason"] = "Changed command"
				if raw, failed := call(operatortool.StopRun, changed); !failed {
					t.Fatalf("changed replay accepted: %s", raw)
				}
				sessionID = ""
				initialized = send("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "stop-client", "version": "1"}})
				sessionID = initialized.Header().Get("Mcp-Session-Id")
				send("notifications/initialized", map[string]any{})
				reconnected, failed := call(operatortool.StopRun, fields)
				var restored struct {
					Preview chat.Action `json:"preview"`
					ID      string      `json:"action_id"`
				}
				if failed || json.Unmarshal(reconnected, &restored) != nil || restored.Preview.Result != result.Preview.Result || restored.ID == "" {
					t.Fatalf("reconnect lost receipt: %s", reconnected)
				}
				polled, failed = call(operatortool.ActionResult, map[string]string{"action_id": restored.ID})
				if failed || !bytes.Equal(reconnected, polled) {
					t.Fatalf("reconnect polling lost receipt: %s", polled)
				}
				if raw, failed := call(operatortool.StopRun, changed); !failed {
					t.Fatalf("changed durable replay accepted: %s", raw)
				}
			}
			current := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
			stopped, found, err := readLease(lease.ID)
			if err != nil || !found || (stopped.session.ReleasedAt != nil) != (scenario == "execute") {
				t.Fatalf("wrong lease released: %+v %v", stopped, err)
			}
			if scenario == "execute" && (current.State != "Todo" || current.Revision != issue.Revision+1 || current.Priority == nil || *current.Priority != 1) {
				t.Fatalf("wrong stop destination: %+v", current)
			}
			if scenario != "execute" && !reflect.DeepEqual(current, issue) {
				t.Fatal("refused stop changed item")
			}
			other, found, err := readLease(otherLease.ID)
			if err != nil || !found || !reflect.DeepEqual(other, otherBefore) || !reflect.DeepEqual(unrelated, readWorkItem(t, f.nativeFixture, unrelated.WorkItemID, "")) {
				t.Fatalf("unrelated item or lease changed: %+v %v", other, err)
			}
			retained := performHubAPIRequest(t, f.service, http.MethodGet, sourcePath, f.token, nil)
			requireNativeStatus(t, retained, http.StatusOK)
			if !bytes.Equal(source.Body.Bytes(), retained.Body.Bytes()) {
				t.Fatal("stop lost published source")
			}
			var transitions int
			if err := query.QueryRowContext(t.Context(), "SELECT count(*) FROM work_events WHERE kind='lease_released'").Scan(&transitions); err != nil {
				t.Fatal(err)
			}
			want := 0
			if scenario == "execute" {
				want = 1
			}
			if transitions != want {
				t.Fatalf("release events=%d, want %d", transitions, want)
			}
		})
	}
}

func TestHostedStopRunAuthority(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, scenario := range []struct {
			name, role     string
			scope          apikey.Scope
			denied, revoke bool
		}{
			{name: "member write", role: "member", scope: apikey.ScopeWrite},
			{name: "owner read", role: "owner", scope: apikey.ScopeRead, denied: true},
			{name: "viewer", role: "viewer", denied: true},
			{name: "reporter", role: "reporter", denied: true},
			{name: "revoked grant", role: "member", scope: apikey.ScopeWrite, revoke: true},
		} {
			t.Run(deployment+"/"+scenario.name, func(t *testing.T) {
				f, ctx := newHostedKeyMCPFixture(t, deployment, scenario.role)
				item := f.seedIssue(t, 1)
				seedCoordinatorArchiveLease(t, f.service, string(item))
				operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Cancelled','Cancelled',1,0,?,?)", f.project, testTimestamp, testTimestamp)
				operatorSQL(t, f.hostedSecurityFixture, `UPDATE projects SET states_json='[{"name":"Todo","dispatchable":true,"transitions":["Cancelled"]},{"name":"Cancelled","terminal":true}]' WHERE id=?`, f.project)
				var fence tracker.FencingToken
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT fencing_token FROM leases WHERE lease_id='archive-lease'").Scan(&fence); err != nil {
					t.Fatal(err)
				}
				if scenario.scope != "" {
					issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: f.user.identity.Hosted, Email: f.user.identity.Email}, apikey.HashToken(f.user.token))
					if err != nil {
						t.Fatal(err)
					}
					expires := time.Now().Add(time.Hour)
					key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "stop-client", Scope: apiScopeOperator, KeyScope: scenario.scope, Issuer: &issuer, ProjectIDs: []string{string(f.project)}, ProjectAccess: hostedProjectsSelected, ExpiresAt: &expires})
					if err != nil {
						t.Fatal(err)
					}
					ctx = changeOperatorContext(t, f.service, key.Token, "org_security")
					connection := operatortool.CurrentConnection(ctx)
					connection.ID = "stop-key-connection"
					ctx = operatortool.WithConnection(ctx, connection)
				}
				executor := hostedOperatorExecutor{f.service}
				if err := executor.OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(operatortool.NativeStopRunArguments{ProjectID: string(f.project), RequestID: "stop", Identifier: string(item), ExpectedRevision: 1, LeaseID: "archive-lease", FencingToken: fence, Destination: "Cancelled"})
				if err != nil {
					t.Fatal(err)
				}
				call := operatortool.Call{Name: operatortool.StopRun, Arguments: raw}
				result, err := executor.Execute(ctx, call)
				if scenario.denied {
					if !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("unauthorized stop: %s %v", result.Content, err)
					}
				} else {
					var receipt struct {
						ID     string            `json:"action_id"`
						Status chat.ActionStatus `json:"status"`
					}
					if err != nil || json.Unmarshal(result.Content, &receipt) != nil || receipt.ID == "" || receipt.Status != chat.ActionSucceeded {
						t.Fatalf("authorized stop: %s %v", result.Content, err)
					}
					if scenario.revoke {
						operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", f.user.identity.Subject, f.project)
						for _, revoked := range []operatortool.Call{call, {Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + receipt.ID + `"}`)}} {
							if _, err := executor.Execute(ctx, revoked); !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("revoked authority accepted %s: %v", revoked.Name, err)
							}
						}
					}
				}
				var released bool
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT released_at IS NOT NULL FROM leases WHERE lease_id='archive-lease'").Scan(&released); err != nil {
					t.Fatal(err)
				}
				if released == scenario.denied {
					t.Fatalf("released=%t denied=%t", released, scenario.denied)
				}
			})
		}
	}
}
