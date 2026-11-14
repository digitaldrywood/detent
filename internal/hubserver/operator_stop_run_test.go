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
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubMCPStopRun(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"execute", "stale revision", "stale fence", "replacement", "foreign project", "foreign item", "missing revision", "missing lease", "missing fence", "release rollback", "routing rollback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			version := f.publish(t, "source", "", true)
			sourcePath := f.path + "/versions/" + version.ID + "/source"
			source := performHubAPIRequest(t, f.service, http.MethodGet, sourcePath, f.token, nil)
			requireNativeStatus(t, source, http.StatusOK)
			unrelated := f.create(t, "unrelated")
			current := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
			transition := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(current.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "start-work"}, ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"})
			requireNativeStatus(t, transition, http.StatusOK)
			decodeHubResponse(t, transition, &current)
			r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.enroll(t)
			otherRunner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			otherRunner.enroll(t)
			for _, runner := range []runnerFixture{r, otherRunner} {
				reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, "Example runner", isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, 1)
			}
			publishCapacity(t, f.nativeFixture, r, capacityReport(f.service.config.Clock()))
			otherCapacity := capacityReport(f.service.config.Clock())
			otherCapacity.AccountAlias, otherCapacity.SharedAccountAlias = "other", "other-team"
			publishCapacity(t, f.nativeFixture, otherRunner, otherCapacity)
			worker := r.redemption.Credential
			claim := func(runner runnerFixture, item tracker.NativeIssue, session string) tracker.NativeLease {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, providerClaim(runner, item, session))
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				return lease
			}
			lease := claim(r, current, "session")
			otherLease := claim(otherRunner, unrelated, "other-session")
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
			readLease := func(id tracker.LeaseID) leasestore.Lease {
				t.Helper()
				var lease leasestore.Lease
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					lease, err = reader.Lease(t.Context(), string(id))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return lease
			}
			active := readLease(lease.ID)
			if active.ReleasedAt != nil {
				t.Fatalf("completed attempt lost active lease: %+v", active)
			}
			otherBefore := readLease(otherLease.ID)
			countTransitions := func() int {
				t.Helper()
				count := 0
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					events, err := reader.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
					for _, event := range events {
						if event.Type == "workflow.transitioned" {
							count++
						}
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return count
			}
			beforeTransitions := countTransitions()
			fields := map[string]any{"project_id": f.project.ID, "request_id": "stop", "identifier": issue.WorkItemID, "expected_revision": fmt.Sprint(issue.Revision), "lease_id": lease.ID, "fencing_token": lease.FencingToken, "destination": "Todo", "priority": 2, "reason": "Release completed attempt"}
			db := usagePostgresAdmin(t, f.service)
			switch scenario {
			case "stale revision":
				fields["expected_revision"] = fmt.Sprint(issue.Revision + 1)
			case "stale fence":
				fields["fencing_token"] = lease.FencingToken + 1
			case "replacement":
				coordinatorActionRelease(t, f.service, f.project.OrganizationID, string(lease.ID), "replaced")
				coordinatorFixtureWrite(t, f.service, f.project.OrganizationID, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
					if err != nil {
						return err
					}
					_, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(r.binding.MachineID), SessionID: "replacement-session", LeaseID: "replacement-lease", RunnerID: r.binding.RunnerID, TTL: time.Hour})
					return err
				})
				issue = readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
				fields["expected_revision"] = fmt.Sprint(issue.Revision)
				lease = tracker.NativeLease{ID: "replacement-lease"}
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
				if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION fail_stop_release() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'release failed'; END $$; CREATE TRIGGER fail_stop_release BEFORE UPDATE OF released_at ON leases FOR EACH ROW EXECUTE FUNCTION fail_stop_release()`); err != nil {
					t.Fatal(err)
				}
			case "routing rollback":
				if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION fail_stop_route() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'routing failed'; END $$; CREATE TRIGGER fail_stop_route BEFORE UPDATE OF workflow_state_name ON issues FOR EACH ROW EXECUTE FUNCTION fail_stop_route()`); err != nil {
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
			current = readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
			if current.ChangeSequence < issue.ChangeSequence {
				t.Fatal("stop regressed project change sequence")
			}
			issue.ChangeSequence = current.ChangeSequence
			stopped := readLease(lease.ID)
			if (stopped.ReleasedAt != nil) != (scenario == "execute") {
				t.Fatalf("wrong lease released: %+v", stopped)
			}
			if scenario == "execute" && (current.State != "Todo" || current.Revision != issue.Revision+1 || current.Priority == nil || *current.Priority != 1) {
				t.Fatalf("wrong stop destination: %+v", current)
			}
			if scenario != "execute" && !reflect.DeepEqual(current, issue) {
				t.Fatal("refused stop changed item")
			}
			other := readLease(otherLease.ID)
			otherItem := readWorkItem(t, f.nativeFixture, unrelated.WorkItemID, "")
			if otherItem.ChangeSequence < unrelated.ChangeSequence {
				t.Fatal("stop regressed unrelated project change sequence")
			}
			unrelated.ChangeSequence = otherItem.ChangeSequence
			if !reflect.DeepEqual(other, otherBefore) || !reflect.DeepEqual(unrelated, otherItem) {
				t.Fatalf("unrelated item or lease changed: %+v", other)
			}
			retained := performHubAPIRequest(t, f.service, http.MethodGet, sourcePath, f.token, nil)
			requireNativeStatus(t, retained, http.StatusOK)
			if !bytes.Equal(source.Body.Bytes(), retained.Body.Bytes()) {
				t.Fatal("stop lost published source")
			}
			transitions := countTransitions() - beforeTransitions
			want := 0
			if scenario == "execute" {
				want = 1
			}
			if transitions != want {
				t.Fatalf("stop transitions=%d, want %d", transitions, want)
			}
		})
	}
}

func TestHostedStopRunAuthority(t *testing.T) {
	t.Parallel()
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
				coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.project)
					if err != nil {
						return err
					}
					project.States = []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}}, {Name: "Cancelled", Terminal: true}}
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				readLease := func() leasestore.Lease {
					t.Helper()
					var lease leasestore.Lease
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						lease, err = r.Lease(t.Context(), "archive-lease")
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return lease
				}
				fence := tracker.FencingToken(readLease().FencingToken)
				if scenario.scope != "" {
					issuer := f.issuer(t)
					expires := time.Now().Add(time.Hour)
					key, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "stop-client", Scope: hubapi.APIScopeOperator, KeyScope: scenario.scope, Issuer: &issuer, ProjectIDs: []string{string(f.project)}, ProjectAccess: hubapi.HostedProjectsSelected, ExpiresAt: &expires})
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
						coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
							return w.DeleteAccessProjectGrant(t.Context(), f.user.identity.Subject, f.project)
						})
						for _, revoked := range []operatortool.Call{call, {Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + receipt.ID + `"}`)}} {
							if _, err := executor.Execute(ctx, revoked); !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("revoked authority accepted %s: %v", revoked.Name, err)
							}
						}
					}
				}
				released := readLease().ReleasedAt != nil
				if released == scenario.denied {
					t.Fatalf("released=%t denied=%t", released, scenario.denied)
				}
			})
		}
	}
}
