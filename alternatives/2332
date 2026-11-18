package stoprun_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type stopRunReceipt struct {
	LeaseID      tracker.LeaseID          `json:"lease_id"`
	FencingToken tracker.FencingToken     `json:"fencing_token"`
	Destination  string                   `json:"destination"`
	WorkItemID   tracker.NativeWorkItemID `json:"work_item_id"`
	Revision     tracker.Revision         `json:"revision,string"`
	State        string                   `json:"state"`
}

func read(t *testing.T, p hubfixture.Native, id tracker.NativeWorkItemID, query string) tracker.NativeIssue {
	t.Helper()
	response := p.Hub.Request(t, http.MethodGet, p.Base+"/work-items/"+string(id)+query, p.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}

func write(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, change func(tenantstore.Writer) error) {
	t.Helper()
	if err := h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(t.Context()); err != nil {
			return err
		}
		return change(w)
	}); err != nil {
		t.Fatal(err)
	}
}

func release(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, leaseID, reason string) {
	t.Helper()
	write(t, h, organization, func(w tenantstore.Writer) error {
		lease, err := w.Lease(t.Context(), leaseID)
		if err != nil {
			return err
		}
		_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, reason)
		return err
	})
}

func TestHubMCPStopRun(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"execute", "stale revision", "stale fence", "replacement", "foreign project", "foreign item", "missing revision", "missing lease", "missing fence", "release rollback", "routing rollback"} {
		t.Run(scenario, func(t *testing.T) {
			f := hubfixture.NewChange(t, nil)
			version := f.PublishSource(t, "source", "", func(input *tracker.ChangeVersionInput) []byte { return sourcetest.Source(t, input) })
			sourcePath := f.Path + "/versions/" + version.ID + "/source"
			source := f.Hub.Request(t, http.MethodGet, sourcePath, f.Token, nil)
			hubfixture.RequireStatus(t, source, http.StatusOK)
			unrelated := f.Create(t, "unrelated")
			current := read(t, f.Native, f.Issue.WorkItemID, "")
			transition := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(current.WorkItemID)+"/workflow", f.Token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "start-work"}, ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"})
			hubfixture.RequireStatus(t, transition, http.StatusOK)
			hubfixture.Decode(t, transition, &current)
			r := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.Enroll(t)
			otherRunner := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			otherRunner.Enroll(t)
			for _, runner := range []hubfixture.Runner{r, otherRunner} {
				hubfixture.ReportRunnerCapacity(t, f.Hub, f.Base, runner.Redemption.Credential, runner.Binding.MachineID, "Example runner", isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, 1)
			}
			hubfixture.PublishCapacity(t, f.Native, r, hubfixture.CapacityReport(f.Hub.Now()))
			otherCapacity := hubfixture.CapacityReport(f.Hub.Now())
			otherCapacity.AccountAlias, otherCapacity.SharedAccountAlias = "other", "other-team"
			hubfixture.PublishCapacity(t, f.Native, otherRunner, otherCapacity)
			worker := r.Redemption.Credential
			claim := func(runner hubfixture.Runner, item tracker.NativeIssue, session string) tracker.NativeLease {
				t.Helper()
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", runner.Redemption.Credential, hubfixture.ProviderClaim(runner, item, session))
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				return lease
			}
			lease := claim(r, current, "session")
			otherLease := claim(otherRunner, unrelated, "other-session")
			start := hubfixture.StartedEvent(lease)
			checkpoint := start
			checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence, checkpoint.Data.Handoff = "run.checkpointed", "checkpoint", 2, hubfixture.Checkpoint()
			finish := start
			finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
			for _, event := range []tracker.NativeRunEvent{start, checkpoint, finish} {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(f.Issue.WorkItemID)+"/events", worker, event), http.StatusOK)
			}
			issue := read(t, f.Native, f.Issue.WorkItemID, "")
			unrelated = read(t, f.Native, unrelated.WorkItemID, "")
			readLease := func(id tracker.LeaseID) leasestore.Lease {
				t.Helper()
				var lease leasestore.Lease
				if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
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
				if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					events, err := reader.Events(t.Context(), f.Project.ID, issue.WorkItemID, 0, 100)
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
			fields := map[string]any{"project_id": f.Project.ID, "request_id": "stop", "identifier": issue.WorkItemID, "expected_revision": fmt.Sprint(issue.Revision), "lease_id": lease.ID, "fencing_token": lease.FencingToken, "destination": "Todo", "priority": 2, "reason": "Release completed attempt"}
			db := f.Hub.Admin
			switch scenario {
			case "stale revision":
				fields["expected_revision"] = fmt.Sprint(issue.Revision + 1)
			case "stale fence":
				fields["fencing_token"] = lease.FencingToken + 1
			case "replacement":
				release(t, f.Hub, f.Project.OrganizationID, string(lease.ID), "replaced")
				write(t, f.Hub, f.Project.OrganizationID, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), f.Project.ID, issue.WorkItemID)
					if err != nil {
						return err
					}
					_, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(r.Binding.MachineID), SessionID: "replacement-session", LeaseID: "replacement-lease", RunnerID: r.Binding.RunnerID, TTL: time.Hour})
					return err
				})
				issue = read(t, f.Native, issue.WorkItemID, "")
				fields["expected_revision"] = fmt.Sprint(issue.Revision)
				lease = tracker.NativeLease{ID: "replacement-lease"}
			case "foreign project":
				foreign := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "foreign")
				fields["project_id"] = foreign.Project.ID
			case "foreign item":
				foreign := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "foreign")
				fields["identifier"] = foreign.Create(t, "foreign").WorkItemID
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
				req := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(f.Project.OrganizationID)+"/mcp", bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+f.Token)
				if sessionID != "" {
					req.Header.Set("Mcp-Session-Id", sessionID)
					req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				}
				response := httptest.NewRecorder()
				f.Hub.Handler().ServeHTTP(response, req)
				status := http.StatusOK
				if strings.HasPrefix(method, "notifications/") {
					status = http.StatusAccepted
				}
				hubfixture.RequireStatus(t, response, status)
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
			hubfixture.Decode(t, listed, &catalog)
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
				hubfixture.Decode(t, response, &envelope)
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
				var receipt stopRunReceipt
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
			current = read(t, f.Native, issue.WorkItemID, "")
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
			otherItem := read(t, f.Native, unrelated.WorkItemID, "")
			if otherItem.ChangeSequence < unrelated.ChangeSequence {
				t.Fatal("stop regressed unrelated project change sequence")
			}
			unrelated.ChangeSequence = otherItem.ChangeSequence
			if !reflect.DeepEqual(other, otherBefore) || !reflect.DeepEqual(unrelated, otherItem) {
				t.Fatalf("unrelated item or lease changed: %+v", other)
			}
			retained := f.Hub.Request(t, http.MethodGet, sourcePath, f.Token, nil)
			hubfixture.RequireStatus(t, retained, http.StatusOK)
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
