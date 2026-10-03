package hubserver

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMCPConfiguredWorkflowTransitions(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(deployment+"/"+transport, func(t *testing.T) {
				f := newHostedKeyMCPFixture(t, deployment, "member")
				f.grant(t, f.user, true, true)
				states := []tracker.NativeState{
					{Name: "Intake", Transitions: []string{"Ready"}},
					{Name: "Ready", Dispatchable: true, Transitions: []string{"Revise", "Ship", "Retired"}},
					{Name: "Revise", Dispatchable: true, Transitions: []string{"Ship"}},
					{Name: "Ship", OperatorOnly: true, Transitions: []string{"Retired"}},
					{Name: "Retired", Terminal: true, Transitions: []string{"Ready"}},
				}
				raw, err := json.Marshal(states)
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(raw), f.project)
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM workflow_states WHERE project_id=?", f.project)
				for _, state := range states {
					operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", f.project, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp)
				}
				send := hostedContextProtocol(t, f.service, f.ctx, transport)
				var catalog struct {
					Tools []operatortool.Definition `json:"tools"`
				}
				if reply := send("tools/list", "", nil); json.Unmarshal(reply.Result, &catalog) != nil || !slices.ContainsFunc(catalog.Tools, func(d operatortool.Definition) bool { return d.Name == operatortool.MoveItem }) {
					t.Fatalf("workflow tool missing: %s %s", reply.Result, reply.Error)
				}
				project := hostedContextData(t, send("tools/call", "get_native_project", map[string]any{"project_id": f.project}), false)
				if !strings.Contains(string(project), `"transitions":["Ready"]`) || !strings.Contains(string(project), `"name":"Retired"`) {
					t.Fatalf("configured state read=%s", project)
				}
				created := hostedContextData(t, send("tools/call", operatortool.FileIssue, map[string]any{"project_id": f.project, "request_id": "create", "title": "Transition", "state": "Intake"}), false)
				var content struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err := json.Unmarshal(created, &content); err != nil || content.Data.WorkItemID == "" {
					t.Fatalf("create=%s %v", created, err)
				}
				id := content.Data.WorkItemID
				itemData := hostedContextData(t, send("tools/call", operatortool.WorkItem, map[string]any{"project_id": f.project, "reference": id}), false)
				var item struct {
					Data operatortool.NativeItem `json:"data"`
				}
				if err := json.Unmarshal(itemData, &item); err != nil || item.Data.Identifier == "" || item.Data.Number <= 0 {
					t.Fatalf("read identifier=%s %v", itemData, err)
				}
				identifier := item.Data.Identifier
				decodeAction := func(data json.RawMessage) chat.Action {
					t.Helper()
					var result struct {
						Preview     chat.Action `json:"preview"`
						ApprovalURL *string     `json:"approval_url"`
					}
					if err := json.Unmarshal(data, &result); err != nil || result.Preview.ID == "" {
						t.Fatalf("transition=%s %v", data, err)
					}
					if result.Preview.Status == chat.ActionPending {
						if result.ApprovalURL == nil || !strings.Contains(*result.ApprovalURL, "connection_id="+result.Preview.ConnectionID) {
							t.Fatalf("pending action lacks exact approval destination: %s", data)
						}
					} else if result.ApprovalURL != nil {
						t.Fatalf("resolved action advertises pending approval: %s", data)
					}
					return result.Preview
				}
				call := func(key, target string, revision int64, denied bool) chat.Action {
					t.Helper()
					data := hostedContextData(t, send("tools/call", operatortool.MoveItem, map[string]any{"project_id": f.project, "request_id": key, "identifier": identifier, "expected_revision": revision, "target_state": target}), denied)
					if denied {
						return chat.Action{}
					}
					return decodeAction(data)
				}
				read := func() tracker.NativeIssue {
					t.Helper()
					issue, _, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(id))
					if err != nil {
						t.Fatal(err)
					}
					return issue
				}
				admission := call("admit", "Ready", 1, false)
				if admission.Status != chat.ActionSucceeded || read().State != "Ready" || read().Revision != 2 {
					t.Fatalf("admission=%+v issue=%+v", admission, read())
				}
				identifier = string(id)
				if replay := call("admit", "Ready", 1, false); replay.ID != admission.ID || replay.Status != chat.ActionSucceeded || read().Revision != 2 {
					t.Fatalf("replay=%+v", replay)
				}
				call("admit", "Revise", 2, true)
				for _, fields := range []map[string]any{
					{"project_id": "prj_foreign", "identifier": id},
					{"project_id": f.project, "identifier": "wi_foreign"},
					{"project_id": f.project, "identifier": "prj_foreign#" + strconv.Itoa(content.Data.Number)},
				} {
					fields["request_id"], fields["target_state"], fields["expected_revision"] = "foreign", "Ready", 1
					hostedContextData(t, send("tools/call", operatortool.MoveItem, fields), true)
				}
				for _, test := range []struct {
					name, target string
					revision     int64
				}{
					{"stale", "Revise", 1},
					{"forbidden", "Intake", 2},
					{"missing state", "Unknown", 2},
				} {
					call(test.name, test.target, test.revision, true)
					if read().State != "Ready" || read().Revision != 2 {
						t.Fatalf("%s changed issue=%+v", test.name, read())
					}
				}
				for i, target := range []string{"Revise", "Ship"} {
					identifier = strconv.Itoa(content.Data.Number)
					if action := call("progress-"+strconv.Itoa(i), target, int64(2+i), false); action.Status != chat.ActionSucceeded || read().State != target || read().Revision != tracker.Revision(3+i) {
						t.Fatalf("request %s: action=%+v issue=%+v", target, action, read())
					}
				}
				pending := call("retire", "Retired", 4, false)
				if pending.Status != chat.ActionPending || read().State != "Ship" || read().Revision != 4 {
					t.Fatalf("terminal approval=%+v issue=%+v", pending, read())
				}
				decision := func(actionID, kind, mode string) {
					t.Helper()
					page := f.browser(http.MethodGet, "/chat/approval?connection_id="+pending.ConnectionID, nil)
					requireNativeStatus(t, page, http.StatusOK)
					fragment := page.Body.String()
					if actionID != "" {
						for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(fragment, -1) {
							if strings.Contains(form, `name="action_id" value="`+actionID+`"`) {
								fragment = form
								break
							}
						}
					}
					form := url.Values{"connection_id": {pending.ConnectionID}, "action_id": {actionID}, "decision": {kind}, "mode": {mode}}
					for _, name := range []string{"csrf", "form_token"} {
						value := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(fragment)
						if len(value) != 2 {
							t.Fatalf("missing %s", name)
						}
						form.Set(name, html.UnescapeString(value[1]))
					}
					requireNativeStatus(t, f.browser(http.MethodPost, "/chat/approval", form), http.StatusSeeOther)
				}
				decodeAction(hostedContextData(t, send("tools/call", operatortool.ActionResult, map[string]any{"action_id": pending.ID}), false))
				decision(pending.ID, "confirm", "")
				decodeAction(hostedContextData(t, send("tools/call", operatortool.ActionResult, map[string]any{"action_id": pending.ID}), false))
				if read().State != "Retired" || read().Revision != 5 {
					t.Fatalf("approved terminal issue=%+v", read())
				}
				reopen := call("reopen", "Ready", 5, false)
				if reopen.Status != chat.ActionPending {
					t.Fatal("terminal source bypassed approval")
				}
				decision("", "mode", "yolo")
				if action := call("reopen-yolo", "Ready", 5, false); action.Status != chat.ActionSucceeded || read().Revision != 6 {
					t.Fatalf("YOLO=%+v issue=%+v", action, read())
				}
				var eventActor tracker.Actor
				var actorJSON, dataJSON string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actor_json,data_json FROM collaboration_events WHERE work_item_id=? AND type='workflow.transitioned' ORDER BY sequence DESC LIMIT 1", id).Scan(&actorJSON, &dataJSON); err != nil {
					t.Fatal(err)
				}
				if json.Unmarshal([]byte(actorJSON), &eventActor) != nil || eventActor.Kind != "human" || eventActor.PrincipalID != operatortool.ConnectionIdentity(f.ctx).PrincipalID || !strings.Contains(dataJSON, `"reason":"user_requested"`) {
					t.Fatalf("history actor=%s data=%s", actorJSON, dataJSON)
				}
				for _, table := range []string{"native_attempts", "leases", "work_events"} {
					var count int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
						t.Fatalf("invented %s: %d %v", table, count, err)
					}
				}
				stored, ok := f.service.operatorChat.Action(admission.ConnectionID, admission.ID)
				if !ok {
					t.Fatal("missing originating action")
				}
				ctx, scope, err := (nativeOperatorExecutor{f.service}).workflowAuthority(f.ctx, stored.Arguments)
				if err != nil {
					t.Fatal(err)
				}
				ctx = mutation.WithContext(ctx, stored.Mutation)
				replay, err := f.service.transitionNativeIssueCommand(ctx, scope, string(id), tracker.Transition{Mutation: tracker.MutationForContext(ctx, ""), ExpectedRevision: 1, State: "Ready", Reason: "user_requested"})
				if err != nil || string(replay) != admission.Result || read().Revision != 6 {
					t.Fatalf("shared workflow owner replay=%s %v", replay, err)
				}
				var receipts int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE operation=?", nativeOperation(scope, "POST", "/work-items/"+string(id)+"/workflow")).Scan(&receipts); err != nil || receipts != 5 {
					t.Fatalf("workflow ledger count=%d %v", receipts, err)
				}
				reconnected := hostedContextProtocol(t, f.service, operatortool.BindConnection(f.ctx, "workflow-reconnect", "test"), transport)
				terminal, ok := f.service.operatorChat.Action(pending.ConnectionID, pending.ID)
				if !ok || terminal.Status != chat.ActionSucceeded {
					t.Fatal("missing genuine terminal receipt")
				}
				for _, original := range []chat.Action{stored, terminal} {
					data := hostedContextData(t, reconnected("tools/call", operatortool.MoveItem, json.RawMessage(original.Arguments)), false)
					var receipt struct {
						Status chat.ActionStatus   `json:"status"`
						Data   tracker.NativeIssue `json:"data"`
					}
					if err := json.Unmarshal(data, &receipt); err != nil || receipt.Status != chat.ActionSucceeded || receipt.Data.State != original.TargetState || receipt.Data.Revision != tracker.Revision(original.Revision) {
						t.Fatalf("durable replay=%s %v", data, err)
					}
				}
				if read().Revision != 6 {
					t.Fatal("reconnect duplicated transition")
				}
				decodeAction(hostedContextData(t, send("tools/call", operatortool.ActionResult, map[string]any{"action_id": admission.ID}), false))
				f.grant(t, f.user, false, false)
				call("admit", "Ready", 1, true)
				call("revoked", "Revise", 6, true)
				hostedContextData(t, send("tools/call", operatortool.ActionResult, map[string]any{"action_id": admission.ID}), true)
				if read().Revision != 6 {
					t.Fatal("revoked request changed issue")
				}
			})
		}
	}
}
