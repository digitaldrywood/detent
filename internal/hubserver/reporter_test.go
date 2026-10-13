package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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
		triage          *tracker.NativeState
	}{
		{"browser/new", "browser", false, nil},
		{"key/new", "key", false, nil},
		{"browser/upgraded", "browser", true, nil},
		{"key/upgraded", "key", true, nil},
		{"browser/terminal intake", "browser", true, &tracker.NativeState{Name: "Triage", Terminal: true, Transitions: []string{"Todo"}}},
		{"key/dispatchable intake", "key", true, &tracker.NativeState{Name: "Triage", Dispatchable: true, OperatorOnly: true, Transitions: []string{"Todo"}}},
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
				previousStates := reporterPreviousHostedStates()
				if test.triage != nil {
					previousStates = append([]tracker.NativeState{*test.triage}, previousStates...)
				}
				previous, err := json.Marshal(previousStates)
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(previous), project.ID)
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM workflow_states WHERE project_id=? AND source_name IN ('Triage','Backlog','Cancelled')", project.ID)
				if test.triage != nil {
					operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Triage','Triage',?,?,?,?)", project.ID, test.triage.Terminal, test.triage.Dispatchable, testTimestamp, testTimestamp)
				}
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
				ScheduledValidation: &gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1", RunAttempt: "1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Conclusion: "success"},
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
			f, ctx := newHostedKeyMCPFixture(t, deployment, "owner")
			catalog, err := operatorCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			member := f.hostedSecurityFixture.user(t, "tester", "member", "tester@example.test", "write", "")
			var membership string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT membership_id FROM hosted_members WHERE user_id=?", member.identity.Subject).Scan(&membership); err != nil {
				t.Fatal(err)
			}
			changed, err := f.service.changeHostedRoleFor(ctx, catalog.credential, membership, "reporter")
			if err != nil || changed.Role != "reporter" {
				t.Fatalf("changed role=%s err=%v", changed.Role, err)
			}
			for _, name := range []string{"first", "second"} {
				invitation, err := f.service.inviteHostedMemberFor(ctx, catalog.credential, name+"@example.test", "reporter", "invite-"+name, []hostedMemberGrant{{ProjectID: string(f.project), Write: true}})
				if err != nil || invitation.Role != "reporter" {
					t.Fatalf("invitation role=%s err=%v", invitation.Role, err)
				}
			}
		})
	}
}

func TestReporterCustomHostedProjectCreation(t *testing.T) {
	for _, test := range []struct {
		name         string
		states, want []tracker.NativeState
		promotion    string
	}{
		{
			name: "conventional destinations",
			states: []tracker.NativeState{
				{Name: "Planned", Transitions: []string{"Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{}},
			},
			want: []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Todo", "Cancelled"}},
				{Name: "Planned", Transitions: []string{"Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{}},
			},
			promotion: "Todo",
		},
		{
			name: "custom destinations",
			states: []tracker.NativeState{
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
			},
			want: []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Ready"}},
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
			},
			promotion: "Ready",
		},
		{
			name: "declared custom intake",
			states: []tracker.NativeState{
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
				{Name: "Triage", OperatorOnly: true, Dispatchable: true, Transitions: []string{"Working"}},
			},
			want: []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Working"}},
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
			},
			promotion: "Working",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, "dedicated", "owner")
			reporter := f.hostedSecurityFixture.user(t, "reporter", "reporter", "reporter@example.test", "", "")
			member := f.hostedSecurityFixture.user(t, "member", "member", "member@example.test", "", "")
			for _, entry := range []string{"JSON", "MCP"} {
				t.Run(entry, func(t *testing.T) {
					var project tracker.NativeProject
					if entry == "JSON" {
						response := f.request(t, f.user, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "create-json", "name": "JSON custom project", "grant_access": true, "states": test.states})
						requireNativeStatus(t, response, http.StatusCreated)
						decodeHubResponse(t, response, &project)
					} else {
						call := projectCall(t, "create_hosted_project", "", "create-mcp", operatortool.HostedProjectCreateInput{Name: "MCP custom project", GrantAccess: true, States: &test.states})
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
					if !reflect.DeepEqual(project.States, test.want) {
						t.Fatalf("workflow=%+v, want %+v", project.States, test.want)
					}
					f.project = project.ID
					f.grantContext(ctx, t, reporter, true, false)
					f.grantContext(ctx, t, member, true, false)
					base := "/api/v2/organizations/org_security/projects/" + string(project.ID)
					for i, state := range []string{"", "Todo", "Planned", "Cancelled", "Triage", "Unknown"} {
						response := f.request(t, reporter, http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("file-%s-%d", entry, i)}, Title: "Custom workflow report", State: state})
						requireNativeStatus(t, response, http.StatusOK)
						var issue tracker.NativeIssue
						decodeHubResponse(t, response, &issue)
						if issue.State != "Triage" {
							t.Fatalf("requested=%q resulting=%q", state, issue.State)
						}
						response = f.request(t, member, http.MethodPost, base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("promote-%s-%d", entry, i)}, ExpectedRevision: issue.Revision, State: test.promotion, Reason: "user_requested"})
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &issue)
						if issue.State != test.promotion {
							t.Fatalf("promotion=%q, want %q", issue.State, test.promotion)
						}
					}
				})
			}
		})
	}
}

func TestNativeProjectCreationWorkflowBounds(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(strconv.FormatBool(hosted), func(t *testing.T) {
			var request func(any) *httptest.ResponseRecorder
			var createdStatus int
			var service *Service
			if hosted {
				f, _ := newHostedKeyMCPFixture(t, "dedicated", "owner")
				service = f.service
				request = func(body any) *httptest.ResponseRecorder {
					return f.request(t, f.user, http.MethodPost, "/api/v2/organizations/org_security/projects", body)
				}
				createdStatus = http.StatusCreated
			} else {
				f := newNativeFixture(t, nil, "", "bounds")
				service = f.service
				request = func(body any) *httptest.ResponseRecorder {
					return performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/projects", testHubAdminToken, body)
				}
				createdStatus = http.StatusOK
			}
			for _, test := range []struct {
				name    string
				count   int
				triage  bool
				allowed bool
			}{
				{"49 custom lanes", 49, false, true},
				{"50 custom lanes", 50, false, false},
				{"50 including Triage", 50, true, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					states := make([]tracker.NativeState, test.count)
					for i := range states {
						states[i] = tracker.NativeState{Name: fmt.Sprintf("Lane %d", i), Transitions: []string{}}
					}
					if test.triage {
						states[0].Name = "Triage"
					}
					body := map[string]any{"idempotency_key": test.name, "name": test.name, "states": states}
					if hosted {
						body["grant_access"] = true
					}
					var before, after int
					if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM projects").Scan(&before); err != nil {
						t.Fatal(err)
					}
					response := request(body)
					if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM projects").Scan(&after); err != nil {
						t.Fatal(err)
					}
					if !test.allowed {
						requireNativeStatus(t, response, http.StatusUnprocessableEntity)
						if after != before {
							t.Fatalf("rejected creation changed project count: %d to %d", before, after)
						}
						return
					}
					requireNativeStatus(t, response, createdStatus)
					if after != before+1 {
						t.Fatalf("creation changed project count: %d to %d", before, after)
					}
					var project tracker.NativeProject
					decodeHubResponse(t, response, &project)
					if len(project.States) != 50 || project.States[0].Name != "Triage" {
						t.Fatalf("workflow=%+v", project.States)
					}
				})
			}
		})
	}
}
