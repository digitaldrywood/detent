package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReporterNativeCommands(t *testing.T) {
	for _, test := range []struct {
		name, transport string
		upgraded        bool
	}{
		{"browser/new", "browser", false},
		{"key/new", "key", false},
		{"browser/upgraded", "browser", true},
		{"key/upgraded", "key", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			u := f.user(t, "reporter", "reporter", "reporter@example.test", "write", "")
			member := f.user(t, "member", "member", "member@example.test", "write", "")
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			created := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/api/v2/organizations/org_security/projects", csrf: cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security"), body: `{"idempotency_key":"default-project","name":"Reporter default project","grant_access":true}`})
			requireNativeStatus(t, created, http.StatusCreated)
			var project tracker.NativeProject
			decodeHubResponse(t, created, &project)
			f.project = project.ID
			f.base = "/api/v2/organizations/org_security/projects/" + string(project.ID)
			if test.upgraded {
				previous, err := json.Marshal(reporterPreviousHostedStates())
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(previous), project.ID)
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM workflow_states WHERE project_id=? AND source_name IN ('Triage','Backlog','Cancelled')", project.ID)
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hub_schema_version WHERE version_id IN (20261013050001,20261013085000)")
				if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
					t.Fatal(err)
				}
			}

			f.grant(t, u, true, false)
			f.grant(t, member, true, false)
			request := func(user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := hostedSharedRequest{user: &user, method: method, target: path, csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")}
				if body != nil {
					r.body = string(raw)
				}
				if test.transport == "key" {
					r.kind = cloudassert.KindKey
					r.mutate = func(c *cloudassert.Claims) {
						c.Subject, c.ProviderOrganization = user.identity.Subject, "org_provider"
						c.Role = "reporter"
						if user.identity.Subject == member.identity.Subject {
							c.Role = "member"
						}
						c.Key = &apikey.KeyAuthority{ID: "key_" + user.identity.Subject, Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{string(f.project)}}}
					}
				}
				return f.serve(t, r)
			}
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{
				Mutation:            tracker.Mutation{IdempotencyKey: "scheduled-validation"},
				ScheduledValidation: &gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1/attempts/1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Conclusion: "success"},
			}), http.StatusForbidden)
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "screenshot.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: artifact.Digest([]byte("screenshot"))}
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/attachment-metadata/check", map[string]any{"size": record.Size}), http.StatusOK)
			attachmentResponse := request(u, http.MethodPost, f.base+"/attachment-metadata", record)
			requireNativeStatus(t, attachmentResponse, http.StatusCreated)
			decodeHubResponse(t, attachmentResponse, &record)
			var own tracker.NativeIssue
			for i, state := range []string{"", "Todo", "Backlog", "Cancelled", "Triage", "Missing"} {
				r := request(u, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("file-%d", i)}, Title: "Tester report", Body: "![Screenshot](attachment:" + record.ID + ")", State: state})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &own)
				if own.State != "Triage" {
					t.Fatalf("requested %q: state=%s", state, own.State)
				}
			}
			attachmentResponse = request(u, http.MethodGet, f.base+"/attachment-metadata/"+record.ID, nil)
			requireNativeStatus(t, attachmentResponse, http.StatusOK)
			var fetched attachment.Metadata
			decodeHubResponse(t, attachmentResponse, &fetched)
			if fetched.ContentType != "image/png" || len(fetched.ReferencedBy) != 6 {
				t.Fatalf("attachment=%#v", fetched)
			}
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/attachment-metadata/"+record.ID+"/reference", map[string]any{"work_item_id": own.WorkItemID}), http.StatusNoContent)
			requireNativeStatus(t, request(u, http.MethodDelete, f.base+"/attachment-metadata/"+record.ID, nil), http.StatusForbidden)
			for _, target := range []string{"Backlog", "Todo", "Cancelled"} {
				var report tracker.NativeIssue
				r := request(u, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "promote-" + target}, Title: "Intake report"})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &report)
				r = request(member, http.MethodPost, f.base+"/work-items/"+string(report.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "promotion-" + target}, ExpectedRevision: report.Revision, State: target, Reason: "user_requested"})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &report)
				if report.State != target {
					t.Fatalf("promoted state=%s", report.State)
				}
			}
			var other tracker.NativeIssue
			r := request(member, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "member-file"}, Title: "Member report"})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &other)
			if other.State != "Todo" {
				t.Fatalf("member default=%s", other.State)
			}
			for _, test := range []struct {
				item   tracker.NativeIssue
				state  string
				status int
			}{
				{own, "Todo", http.StatusForbidden},
				{other, "Cancelled", http.StatusForbidden},
				{own, "Cancelled", http.StatusOK},
			} {
				target := f.base + "/work-items/" + string(test.item.WorkItemID) + "/workflow"
				r := request(u, http.MethodPost, target, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-" + string(test.item.WorkItemID) + test.state}, ExpectedRevision: test.item.Revision, State: test.state, Reason: "user_requested"})
				requireNativeStatus(t, r, test.status)
			}
			var comment tracker.NativeComment
			commentPath := f.base + "/work-items/" + string(other.WorkItemID) + "/comments"
			r = request(u, http.MethodPost, commentPath, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Tester observation"})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &comment)
			r = request(u, http.MethodPatch, commentPath+"/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-own"}, ExpectedRevision: comment.Revision, Body: "Updated observation"})
			requireNativeStatus(t, r, http.StatusOK)
			r = request(member, http.MethodPost, commentPath, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "member-comment"}, Body: "Operator observation"})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &comment)
			r = request(u, http.MethodPatch, commentPath+"/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-other"}, ExpectedRevision: comment.Revision, Body: "Replacement"})
			requireNativeStatus(t, r, http.StatusForbidden)
			for _, suffix := range []string{"", "/comments", "/history"} {
				r := request(u, http.MethodGet, f.base+"/work-items/"+string(other.WorkItemID)+suffix, nil)
				requireNativeStatus(t, r, http.StatusOK)
			}
			for _, test := range []struct{ method, suffix string }{{http.MethodPatch, ""}, {http.MethodPost, "/dependencies"}, {http.MethodPost, "/restore"}, {http.MethodPost, "/archive"}} {
				r := request(u, test.method, f.base+"/work-items/"+string(other.WorkItemID)+test.suffix, map[string]any{"idempotency_key": "denied"})
				requireNativeStatus(t, r, http.StatusForbidden)
			}
			operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", u.identity.Subject)
			requireNativeStatus(t, request(u, http.MethodGet, f.base+"/work-items", nil), http.StatusNotFound)
		})
	}
}

func TestReporterMCPFilesIntoTriage(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "reporter")
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: "org_security", project: f.project}, nativeFixtureStates(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			executor := hostedOperatorExecutor{f.service}
			definitions, err := executor.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, definition := range definitions {
				seen[definition.Name] = true
			}
			for _, name := range []string{operatortool.FileIssue, operatortool.AddComment, operatortool.EditComment, operatortool.MoveItem} {
				if !seen[name] {
					t.Fatalf("Reporter catalog omits %s", name)
				}
			}
			for _, name := range []string{operatortool.EditItem, operatortool.SetDependency, operatortool.RestoreItem, operatortool.DeleteAttachment} {
				if seen[name] {
					t.Fatalf("Reporter catalog advertises %s", name)
				}
			}
			for _, definition := range operatortool.AdministrationCatalog() {
				if seen[definition.Name] {
					t.Fatalf("Reporter catalog advertises administration tool %s", definition.Name)
				}
			}

			for i, state := range []string{"", "Todo", "Cancelled", "Triage", "Missing"} {
				raw, err := json.Marshal(map[string]any{"project_id": f.project, "request_id": fmt.Sprintf("file-%d", i), "title": "Tester report", "state": state})
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				var content struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Data.State != "Triage" {
					t.Fatalf("result=%s", result.Content)
				}
			}
		})
	}
}

func TestReporterPersonalKeyIntersection(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "reporter")
			catalog, err := operatorCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"first", "second"} {
				key, err := f.service.createHostedAPIKeyFor(ctx, catalog.credential, hostedKeyRequest{Name: name, Scope: apikey.ScopeWrite, Days: 30, Projects: []string{string(f.project)}})
				if err != nil {
					t.Fatal(err)
				}
				send := func(method, path string, body any) *httptest.ResponseRecorder {
					t.Helper()
					if deployment == "shared" {
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						shared := hostedSharedFixture{hostedSecurityFixture: f.hostedSecurityFixture, key: hostedSharedKey(7)}
						return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: method, target: path, body: string(raw), bearer: key.Token})
					}
					return performHubAPIRequest(t, f.service, method, path, key.Token, body)
				}
				r := send(http.MethodGet, f.base+"/work-items", nil)
				requireNativeStatus(t, r, http.StatusOK)
				r = send(http.MethodPatch, f.base+"/work-items/wi_other", map[string]any{"idempotency_key": "denied"})
				requireNativeStatus(t, r, http.StatusForbidden)
			}
			if _, err := f.service.createHostedAPIKeyFor(ctx, catalog.credential, hostedKeyRequest{Name: "admin", Scope: apikey.ScopeAdmin, Days: 30}); err == nil {
				t.Fatal("reporter created admin key")
			}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", f.user.identity.Subject)
			_, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(f.project), Action: operatortool.FileIssue})
			if err == nil {
				t.Fatal("stale reporter authority survived local downgrade")
			}
		})
	}
}

func TestReporterRoleAssignment(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, _ := newHostedKeyMCPFixture(t, deployment, "owner")
			member := f.hostedSecurityFixture.user(t, "tester", "member", "tester@example.test", "write", "")
			var membership string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT membership_id FROM hosted_members WHERE user_id=?", member.identity.Subject).Scan(&membership); err != nil {
				t.Fatal(err)
			}
			call := func(name string, arguments map[string]any, target any) {
				t.Helper()
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "Reporter assignment", "version": "1"}}}})
				if err != nil {
					t.Fatal(err)
				}
				response := reporterEntryRequest(t, f, deployment, "", "web-session", http.MethodPost, "/api/v2/organizations/org_security/mcp", raw, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": name})
				requireNativeStatus(t, response, http.StatusOK)
				var frame struct {
					Result struct {
						IsError    bool `json:"isError"`
						Structured struct {
							Action chatpkg.Action  `json:"action"`
							Output json.RawMessage `json:"output"`
						} `json:"structuredContent"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &frame)
				if frame.Result.IsError || frame.Result.Structured.Action.Status != chatpkg.ActionSucceeded {
					t.Fatalf("%s failed: %s", name, response.Body.String())
				}
				if err := json.Unmarshal(frame.Result.Structured.Output, target); err != nil {
					t.Fatal(err)
				}
			}
			var changed struct {
				Data hostedMemberView `json:"data"`
			}
			call(operatortool.MemberRole, map[string]any{"request_id": "assign-reporter", "member_id": membership, "role": "reporter"}, &changed)
			if changed.Data.Role != "reporter" {
				t.Fatalf("changed role=%s", changed.Data.Role)
			}
			for _, name := range []string{"first", "second"} {
				var invitation struct {
					Data hostedInvitationView `json:"data"`
				}
				call(operatortool.InvitationSend, map[string]any{"request_id": "invite-" + name, "email": name + "@example.test", "role": "reporter", "grants": []hostedMemberGrant{{ProjectID: string(f.project), Write: true}}}, &invitation)
				if invitation.Data.Role != "reporter" {
					t.Fatalf("invitation role=%s", invitation.Data.Role)
				}
			}
			var role string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT role FROM hosted_members WHERE membership_id=?", membership).Scan(&role); err != nil || role != "reporter" {
				t.Fatalf("persisted role=%s err=%v", role, err)
			}

		})
	}
}

func TestReporterCustomHostedProjectCreation(t *testing.T) {
	f, ctx := newHostedKeyMCPFixture(t, "dedicated", "owner")
	reporter := f.hostedSecurityFixture.user(t, "reporter", "reporter", "reporter@example.test", "", "")
	states := []tracker.NativeState{
		{Name: "Planned", Transitions: []string{"Todo"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}},
		{Name: "Cancelled", Terminal: true, Transitions: []string{}},
	}
	for _, entry := range []string{"JSON", "MCP"} {
		t.Run(entry, func(t *testing.T) {
			var project tracker.NativeProject
			if entry == "JSON" {
				response := f.request(t, f.user, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "create-json", "name": "JSON custom project", "grant_access": true, "states": states})
				requireNativeStatus(t, response, http.StatusCreated)
				decodeHubResponse(t, response, &project)
			} else {
				call := projectCall(t, "create_hosted_project", "", "create-mcp", operatortool.HostedProjectCreateInput{Name: "MCP custom project", GrantAccess: true, States: &states})
				rawResult, err := (hubProjectExecutor{service: f.service}).Execute(ctx, call)
				if err != nil {
					t.Fatalf("creation error=%T %+v", err, err)
				}
				var result chatpkg.Action
				if err := json.Unmarshal(rawResult.Content, &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != chatpkg.ActionSucceeded {
					t.Fatalf("creation=%+v", result)
				}
				if err := json.Unmarshal([]byte(result.Result), &project); err != nil {
					t.Fatal(err)
				}
			}
			want := []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Todo", "Cancelled"}},
				{Name: "Planned", Transitions: []string{"Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{}},
			}
			if !reflect.DeepEqual(project.States, want) {
				t.Fatalf("workflow=%+v, want %+v", project.States, want)
			}
			f.project = project.ID
			f.grantContext(ctx, t, reporter, true, false)
			base := "/api/v2/organizations/org_security/projects/" + string(project.ID)
			for i, state := range []string{"", "Todo", "Planned", "Cancelled", "Triage", "Unknown"} {
				response := f.request(t, reporter, http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("file-%s-%d", entry, i)}, Title: "Custom workflow report", State: state})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				if issue.State != "Triage" {
					t.Fatalf("requested=%q resulting=%q", state, issue.State)
				}
			}
		})
	}
}
