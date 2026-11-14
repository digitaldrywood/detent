package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedProjectDeletion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, account, update, confirm, organization, role string
		noCSRF                                             bool
		status                                             int
	}{
		{name: "owner deletes idle project", account: "owner", status: http.StatusOK},
		{name: "admin deletes idle project", account: "owner", role: "admin", status: http.StatusOK},
		{name: "member with write grant refused", account: "owner", role: "member", update: "WITH granted AS (INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner,explicit) SELECT organization_id,'user_browser_owner',id,TRUE,FALSE,TRUE FROM projects WHERE organization_id='org_browser_preview' AND deleted_at IS NULL) INSERT INTO token_grants(organization_id,token_id,project_id) SELECT p.organization_id,m.principal_id,p.id FROM projects p JOIN hosted_members m ON m.organization_id=p.organization_id AND m.user_id='user_browser_owner' WHERE p.organization_id='org_browser_preview' AND p.deleted_at IS NULL AND p.id<>$1::text||'-' ON CONFLICT DO NOTHING", status: http.StatusForbidden},
		{name: "viewer refused", account: "viewer", status: http.StatusForbidden},
		{name: "cross organization session refused", account: "wrong-organization", status: http.StatusForbidden},
		{name: "cross organization path refused", account: "owner", organization: "org_other", status: http.StatusNotFound},
		{name: "owner with read-only grant row deletes", account: "owner", update: "INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner,explicit) VALUES('org_browser_preview','user_browser_owner',$1,FALSE,FALSE,TRUE)", status: http.StatusOK},
		{name: "missing CSRF refused", account: "owner", noCSRF: true, status: http.StatusForbidden},
		{name: "wrong confirmation refused", account: "owner", confirm: "Wrong project", status: http.StatusUnprocessableEntity},
		{name: "active lease refused", account: "owner", update: "UPDATE leases SET released_at=NULL,expires_at='2999-01-01T00:00:00Z' WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1)", status: http.StatusUnprocessableEntity},
		{name: "running attempt with its live lease refused", account: "owner", update: "WITH attempts AS (UPDATE native_attempts SET status='running' WHERE project_id=$1 RETURNING issue_id) UPDATE leases SET released_at=NULL,expires_at='2999-01-01T00:00:00Z' WHERE issue_id IN (SELECT issue_id FROM attempts)", status: http.StatusUnprocessableEntity},
		{name: "running attempt without a live lease deletes", account: "owner", update: "UPDATE native_attempts SET status='running' WHERE project_id=$1", status: http.StatusOK},
		{name: "open workspace refused", account: "owner", update: "UPDATE workspace_sessions SET state='ready' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "queued script refused", account: "owner", update: "UPDATE project_action_runs SET status='queued' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "running script refused", account: "owner", update: "UPDATE project_action_runs SET status='running' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "chat with unknown execution deletes", account: "owner", update: "UPDATE conversations SET execution_status=NULL WHERE project_id=$1", status: http.StatusOK},
		{name: "chat reporting a run without a lease deletes", account: "owner", update: "UPDATE conversations SET execution_status='running' WHERE project_id=$1", status: http.StatusOK},
		{name: "chat turn without a lease deletes", account: "owner", update: "UPDATE conversations SET execution_status='running',execution_turn_id='deletion-turn' WHERE project_id=$1", status: http.StatusOK},
		{name: "chat with a queued message deletes", account: "owner", update: "UPDATE conversation_messages SET delivery='queued' WHERE conversation_id IN (SELECT id FROM conversations WHERE project_id=$1)", status: http.StatusOK},
		{name: "pending import refused", account: "owner", update: "UPDATE github_imports SET status='pending' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "partial import refused", account: "owner", update: "UPDATE github_imports SET status='partial' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "pending GitHub write refused", account: "owner", update: "UPDATE github_outbound SET status='pending' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "processing GitHub write refused", account: "owner", update: "UPDATE github_outbound SET status='processing' WHERE project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "organization Sprite preserved", account: "owner", update: "UPDATE runner_sprite_memberships SET state='enrolled',token_project_id='' WHERE token_project_id=$1", status: http.StatusOK},
		{name: "project allocation using organization token refused", account: "owner", update: "UPDATE runner_sprite_memberships SET state='enrolled',allocation_project_id=token_project_id,token_project_id='' WHERE token_project_id=$1", status: http.StatusUnprocessableEntity},
		{name: "provisioned Sprite using project token refused", account: "owner", update: "UPDATE runner_sprite_memberships SET state='enrolled' WHERE token_project_id=$1", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
			db := hostedPostgresAdmin(t, f.service)
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			var item, principal string
			if err := db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE organization_id='org_browser_preview' AND project_id=$1", f.project).Scan(&item); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE organization_id='org_browser_preview' AND user_id='user_browser_owner'").Scan(&principal); err != nil {
				t.Fatal(err)
			}
			organization := tracker.OrganizationID("org_browser_preview")
			scope := hubscope.NativeScope{Organization: organization, Project: tracker.ProjectID(f.project)}
			at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			seedAnalyticsTestAttempt(t, db, f.service, scope, tracker.NativeWorkItemID(item), "deletion-attempt", "succeeded", at, at.Add(time.Minute), tracker.NativeRunData{})
			seedAnalyticsTestChange(t, f.service, scope, tracker.NativeWorkItemID(item), "deletion-change", at, nil)
			command, err := eventstore.NewContent(f.service.runDetailStore()).PreparePayload(t.Context(), organization, []byte("go test"))
			if err != nil {
				t.Fatal(err)
			}
			exec("UPDATE project_settings SET repository='example-org/example-project',checkout_repository='example-org/example-project',repository_enabled=TRUE WHERE organization_id='org_browser_preview' AND project_id IN ($1,$2)", f.project, f.privateProject)
			exec(`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,created_at,updated_at,expires_at,native_only) VALUES('org_browser_preview','shared-runner-token','shared-runner',$1,'runner','worker',$2,$2,'2999-01-01T00:00:00Z',TRUE)`, strings.Repeat("c", 64), at)
			exec(`INSERT INTO runner_enrollments(id,organization_id,runner_id,machine_id,token_hash,scope,operations,created_at,expires_at,created_by,redeemed_at) VALUES('deletion-enrollment','org_browser_preview','shared-runner','analytics-fixture',$1,'projects',ARRAY['claim'],$2,'2999-01-01T00:00:00Z',$3,$2)`, strings.Repeat("a", 64), at, principal)
			exec(`INSERT INTO runner_identities(runner_id,organization_id,machine_id,token_id,enrollment_id,scope,operations,created_at,expires_at,capacity_limit,reported_capacity,last_heartbeat_at) VALUES('shared-runner','org_browser_preview','analytics-fixture','shared-runner-token','deletion-enrollment','projects',ARRAY['claim'],$1,'2999-01-01T00:00:00Z',1,1,$1)`, at)
			exec(`INSERT INTO runner_repositories(organization_id,runner_id,owner,name,private,listed_at) VALUES('org_browser_preview','shared-runner','example-org','example-project',FALSE,$1)`, at)
			exec(`INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) VALUES('deletion-workspace','org_browser_preview',$1,$2,'branch','closed','{}',300,$3,$3,$4,$5,$5)`, f.project, item, at.Add(time.Hour), principal, at)
			exec(`INSERT INTO project_actions(id,organization_id,project_id,slot,name,command_object_key,command_sha256,command_bytes,command_content_bytes,created_by,revision,created_at,updated_at) VALUES('deletion-action','org_browser_preview',$1,0,'Test',$2,$3,$4,$5,$6,1,$7,$7)`, f.project, command.Key, command.SHA256, command.CompressedSize, command.UncompressedSize, principal, at)
			exec(`INSERT INTO project_action_runs(id,action_id,organization_id,project_id,workspace_id,command_object_key,command_sha256,command_bytes,command_content_bytes,status,created_by,created_at,updated_at) VALUES('deletion-script','deletion-action','org_browser_preview',$1,'deletion-workspace',$2,$3,$4,$5,'succeeded',$6,$7,$7)`, f.project, command.Key, command.SHA256, command.CompressedSize, command.UncompressedSize, principal, at)
			exec(`INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,origin,visibility,status,execution_status,execution_lease_id,execution_settled,created_at,updated_at) VALUES('deletion-chat','org_browser_preview',$1,$2,'user','private','active','idle','',TRUE,$3,$3)`, f.project, principal, at)
			exec(`INSERT INTO conversation_messages(id,organization_id,conversation_id,seq,role,kind,delivery,actor_kind,actor_principal_id,text_bytes,stored_text_bytes,created_at,updated_at) VALUES('deletion-message','org_browser_preview','deletion-chat',1,'user','text','completed','human',$1,0,0,$2,$2)`, principal, at)
			exec(`INSERT INTO github_imports(id,organization_id,project_id,issue_number,stage,status,observed_at) VALUES('deletion-import','org_browser_preview',$1,1,'complete','retrieved',$2)`, f.project, at)
			exec(`INSERT INTO github_outbound(organization_id,project_id,work_item_id,command_key,repository,issue_number,kind,status,next_attempt_at,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES('org_browser_preview',$1,$2,'deletion-outbound','example-org/example-project',1,'issue_comment','completed',$3,$3,$4,$5,$6,$7)`, f.project, item, at, command.Key, command.SHA256, command.CompressedSize, command.UncompressedSize)
			exec(`INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,work_item_id) VALUES('deletion-attachment','org_browser_preview',$1,$2,'file.txt','text/plain',1,$3,$4,$5)`, f.project, principal, strings.Repeat("b", 64), at, item)
			exec(`INSERT INTO artifact_services(organization_id,project_id,id,origin,binding_mode,hosted_opt_in,publisher_token_id) VALUES('org_browser_preview',$1,'deletion-service','https://artifacts.example.test','customer',FALSE,$2)`, f.project, principal)
			exec(`INSERT INTO artifact_references(organization_id,project_id,work_item_id,artifact_id,revision,service_id,manifest_id,schema_version,sha256,kind,state,availability,bytes,objects,expires_at,observed_at) VALUES('org_browser_preview',$1,$2,'deletion-artifact',1,'deletion-service','manifest',1,$3,'log','complete','available',0,0,$4,$5)`, f.project, item, strings.Repeat("b", 64), at.Add(time.Hour), at)
			for _, project := range []string{f.project, f.privateProject} {
				exec(`INSERT INTO token_grants(token_id,organization_id,project_id) VALUES('shared-runner-token','org_browser_preview',$1)`, project)
				exec(`INSERT INTO runner_enrollment_projects(enrollment_id,organization_id,project_id) VALUES('deletion-enrollment','org_browser_preview',$1)`, project)
				exec(`INSERT INTO project_secrets(organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES('org_browser_preview',$1,'fly_sprites_token','retained',$2,$3,$4,1,$5)`, project, []byte{1}, []byte{2}, []byte{3}, at)
			}
			exec(`INSERT INTO organization_sprite_pools(organization_id,min_runners,max_runners,idle_seconds,configured_by,revision,isolation_tier,share_idle_capacity,placement_mode,todo_threshold,overflow_slots,max_sprite_slots) VALUES('org_browser_preview',0,1,300,$1,1,'sandbox',FALSE,'blended',0,0,1)`, principal)
			exec(`INSERT INTO runner_sprite_memberships(organization_id,token_project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES('org_browser_preview',$1,'sprite','org','deletion-enrollment','deleted',$2,$2)`, f.project, at)
			if test.role != "" {
				var membership string
				if err := db.QueryRowContext(t.Context(), "SELECT membership_id FROM hosted_members WHERE user_id='user_browser_owner'").Scan(&membership); err != nil {
					t.Fatal(err)
				}
				if err := f.provider.SetMembershipRole(t.Context(), membership, test.role); err != nil {
					t.Fatal(err)
				}
				exec("UPDATE hosted_members SET role=$1 WHERE organization_id='org_browser_preview' AND user_id='user_browser_owner'", test.role)
			}
			if test.update != "" {
				exec(test.update, f.project)
			}
			other := f.rawAPI(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/projects/"+f.privateProject+"/work-items", "{\"body\":\"\\n## Acceptance criteria\\nComplete the requested work.\\n## Must not break\\nExisting behavior.\\n## How we know it worked\\nRun the project checks.\",\"idempotency_key\":\"other-project-item\",\"state\":\"Todo\",\"title\":\"Other project active work\"}", map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies["owner"].Value)})
			browserHostedStatus(t, other, http.StatusOK)
			exec(`INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) SELECT 'other-workspace',organization_id,project_id,native_id,'branch','requested','{}',300,'2999-01-01T00:00:00Z','2999-01-01T00:00:00Z',$1,$2,$2 FROM issues WHERE organization_id='org_browser_preview' AND project_id=$3`, principal, at, f.privateProject)
			before := deletionDataSnapshot(t, db)
			organizationPath, confirm := test.organization, test.confirm
			if organizationPath == "" {
				organizationPath = "org_browser_preview"
			}
			if confirm == "" {
				confirm = "Browser collaboration"
			}
			body, err := json.Marshal(map[string]string{"idempotency_key": "delete-project", "confirm_name": confirm})
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{}
			if !test.noCSRF {
				headers["X-CSRF-Token"] = hostedCSRF(f.cookies[test.account].Value)
			}
			base := "/api/v2/organizations/" + organizationPath + "/projects/" + f.project
			response := f.rawAPI(t, test.account, http.MethodDelete, base, string(body), headers)
			browserHostedStatus(t, response, test.status)
			if test.role == "member" && !strings.Contains(response.Body.String(), `"code":"access_denied"`) {
				t.Fatalf("visible member deletion refusal = %s", response.Body.String())
			}
			if test.status != http.StatusOK {
				if after := deletionDataSnapshot(t, db); before != after {
					t.Fatalf("refused deletion changed project data\nbefore: %s\nafter: %s", before, after)
				}
				return
			}
			browserHostedStatus(t, f.page(t, "owner", "/projects/"+f.project), http.StatusNotFound)
			browserHostedStatus(t, f.page(t, "owner", "/projects/"+f.project+"/settings"), http.StatusNotFound)
			var beforeRecords, afterRecords map[string]json.RawMessage
			if err := json.Unmarshal([]byte(before), &beforeRecords); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(deletionDataSnapshot(t, db)), &afterRecords); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"issues", "native_attempts", "workspace_sessions", "project_action_runs", "conversations", "conversation_messages", "github_imports", "github_outbound", "runner_sprite_memberships", "organization_sprite_pools", "runner_repositories", "machines", "runner_identities", "organizations", "attachments", "artifact_references", "change_requests", "change_versions"} {
				if string(beforeRecords[table]) != string(afterRecords[table]) {
					t.Fatalf("deletion changed retained %s records", table)
				}
			}
			for _, path := range []string{base + "/integration", base + "/work-items", base + "/work-items/" + item, "/api/v2/organizations/org_browser_preview/work-items/" + item} {
				browserHostedStatus(t, f.rawAPI(t, "owner", http.MethodGet, path, "", nil), http.StatusNotFound)
			}
			browserHostedStatus(t, f.rawAPI(t, "owner", http.MethodDelete, base, string(body), headers), http.StatusNotFound)
			for _, path := range []string{browserHostedOrganizationBase + "/projects", browserHostedOrganizationBase + "/project-rank", "/app/bootstrap"} {
				response := f.rawAPI(t, "owner", http.MethodGet, path, "", nil)
				browserHostedStatus(t, response, http.StatusOK)
				if strings.Contains(response.Body.String(), f.project) || !strings.Contains(response.Body.String(), f.privateProject) {
					t.Fatalf("project list did not isolate deletion: %s", response.Body.String())
				}
			}
			for query, want := range map[string]int{
				"SELECT count(*) FROM projects WHERE deleted_at IS NOT NULL":                      1,
				"SELECT count(*) FROM project_secrets":                                            1,
				"SELECT count(*) FROM hosted_project_grants WHERE project_id='" + f.project + "'": 0,
				"SELECT count(*) FROM token_grants WHERE project_id='" + f.project + "'":          0,
				"SELECT count(*) FROM runner_enrollment_projects":                                 1,
				"SELECT count(*) FROM issues":                                                     2,
				"SELECT count(*) FROM native_attempts":                                            1,
				"SELECT count(*) FROM project_action_runs":                                        1,
				"SELECT count(*) FROM conversations":                                              1,
				"SELECT count(*) FROM runner_repositories":                                        1,
				"SELECT count(*) FROM github_outbound WHERE status='completed'":                   1,
				"SELECT count(*) FROM machines":                                                   1,
				"SELECT count(*) FROM runner_enrollments":                                         1,
			} {
				var got int
				if err := db.QueryRowContext(t.Context(), query).Scan(&got); err != nil || got != want {
					t.Fatalf("%s = %d, want %d: %v", query, got, want, err)
				}
			}
		})
	}
}

func deletionDataSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	data := map[string][]json.RawMessage{}
	for _, table := range []string{"projects", "project_settings", "issues", "native_attempts", "workspace_sessions", "project_action_runs", "conversations", "conversation_messages", "github_imports", "github_outbound", "project_secrets", "hosted_project_grants", "token_grants", "runner_enrollment_projects", "runner_sprite_memberships", "organization_sprite_pools", "runner_repositories", "machines", "runner_identities", "organizations", "attachments", "artifact_references", "change_requests", "change_versions"} {
		rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error(err)
			}
		}()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			data[table] = append(data[table], encoded)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(data[table], func(a, b json.RawMessage) int { return bytes.Compare(a, b) })
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
