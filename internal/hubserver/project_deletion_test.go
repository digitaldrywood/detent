package hubserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHostedProjectDeletion(t *testing.T) {
	for _, test := range []struct {
		name, account, update, confirm, organization, role string
		noCSRF                                             bool
		status                                             int
	}{
		{name: "owner deletes idle project", account: "owner", status: http.StatusOK},
		{name: "admin deletes idle project", account: "owner", role: "admin", status: http.StatusOK},
		{name: "member with write grant refused", account: "owner", role: "member", status: http.StatusNotFound},
		{name: "viewer refused", account: "viewer", status: http.StatusNotFound},
		{name: "cross organization session refused", account: "wrong-organization", status: http.StatusForbidden},
		{name: "cross organization path refused", account: "owner", organization: "org_other", status: http.StatusNotFound},
		{name: "missing write grant refused", account: "owner", update: "UPDATE hosted_project_grants SET can_write=0 WHERE project_id=?", status: http.StatusNotFound},
		{name: "missing CSRF refused", account: "owner", noCSRF: true, status: http.StatusForbidden},
		{name: "wrong confirmation refused", account: "owner", confirm: "Wrong project", status: http.StatusUnprocessableEntity},
		{name: "active lease refused", account: "owner", update: "UPDATE leases SET released_at=NULL,expires_at='2999-01-01T00:00:00Z' WHERE issue_id IN (SELECT id FROM issues WHERE project_id=?)", status: http.StatusUnprocessableEntity},
		{name: "running attempt refused", account: "owner", update: "UPDATE native_attempts SET status='running' WHERE project_id=?", status: http.StatusUnprocessableEntity},
		{name: "open workspace refused", account: "owner", update: "UPDATE workspace_sessions SET state='ready' WHERE project_id=?", status: http.StatusUnprocessableEntity},
		{name: "queued script refused", account: "owner", update: "UPDATE project_action_runs SET status='queued' WHERE project_id=?", status: http.StatusUnprocessableEntity},
		{name: "running script refused", account: "owner", update: "UPDATE project_action_runs SET status='running' WHERE project_id=?", status: http.StatusUnprocessableEntity},
		{name: "conversation turn refused", account: "owner", update: `UPDATE conversations SET execution_json='{"status":"running"}' WHERE project_id=?`, status: http.StatusUnprocessableEntity},
		{name: "queued conversation refused", account: "owner", update: "UPDATE conversation_messages SET delivery='queued' WHERE conversation_id IN (SELECT id FROM conversations WHERE project_id=?)", status: http.StatusUnprocessableEntity},
		{name: "pending import refused", account: "owner", update: "UPDATE github_imports SET status='pending' WHERE project_id=?", status: http.StatusUnprocessableEntity},
		{name: "partial import refused", account: "owner", update: "UPDATE github_imports SET status='partial' WHERE project_id=?", status: http.StatusUnprocessableEntity},
		{name: "pending GitHub write refused", account: "owner", update: "UPDATE github_outbox SET status='pending' WHERE issue_id IN (SELECT id FROM issues WHERE project_id=?)", status: http.StatusUnprocessableEntity},
		{name: "processing GitHub write refused", account: "owner", update: "UPDATE github_outbox SET status='processing' WHERE issue_id IN (SELECT id FROM issues WHERE project_id=?)", status: http.StatusUnprocessableEntity},
		{name: "provisioned Sprite refused", account: "owner", update: "UPDATE project_sprite_members SET state='enrolled' WHERE project_id=?", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
			db := f.service.database.db
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			var item, principal string
			var issueID int64
			if err := db.QueryRowContext(t.Context(), "SELECT id,native_id FROM issues WHERE project_id=?", f.project).Scan(&issueID, &item); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id='user_browser_owner'").Scan(&principal); err != nil {
				t.Fatal(err)
			}
			exec("INSERT INTO repositories(id,github_node_id,github_owner,github_name,created_at,updated_at) VALUES(987,'repo-deletion','acme','retained','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')")
			exec("UPDATE projects SET repository_id=987,checkout_repository='acme/retained',github_repository_enabled=1 WHERE id=?", f.project)
			exec("INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at,expires_at,native_only) VALUES('shared-runner-token','shared-runner',?,'runner','worker','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2999-01-01T00:00:00Z',1)", strings.Repeat("c", 64))
			exec("INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id,token_id) VALUES('deletion-machine','host',1,'1','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','org_browser_preview','shared-runner-token')")
			exec("INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES('deletion-lease',?,'deletion-machine','deletion-session','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')", issueID)
			exec("INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) SELECT 'deletion-attempt',organization_id,project_id,native_id,'deletion-lease',(SELECT fencing_token FROM leases WHERE lease_id='deletion-lease'),'run',1,'succeeded','{}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM issues WHERE id=?", issueID)
			exec("INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) SELECT 'deletion-workspace',organization_id,project_id,native_id,'branch','closed',300,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM issues WHERE id=?", principal, issueID)
			exec("INSERT INTO project_actions(id,organization_id,project_id,name,command,created_by,created_at,updated_at) SELECT 'deletion-action',organization_id,id,'Test','go test',?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM projects WHERE id=?", principal, f.project)
			exec("INSERT INTO project_action_runs(id,action_id,organization_id,project_id,workspace_id,command,status,created_at,updated_at) SELECT 'deletion-script','deletion-action',organization_id,id,'deletion-workspace','go test','succeeded','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM projects WHERE id=?", f.project)
			exec(`INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,visibility,status,execution_json,created_at,updated_at) SELECT 'deletion-chat',organization_id,id,?,'private','active','{"status":"idle"}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM projects WHERE id=?`, principal, f.project)
			exec(`INSERT INTO conversation_messages(id,conversation_id,seq,role,kind,delivery,actor_json,created_at,updated_at) VALUES('deletion-message','deletion-chat',1,'user','text','completed','{"kind":"human","principal_id":"owner"}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
			exec("INSERT INTO github_imports(id,project_id,issue_number,status,observed_at) VALUES('deletion-import',?,1,'retrieved','2026-01-01T00:00:00Z')", f.project)
			exec("INSERT INTO github_outbox(idempotency_key,repository_id,issue_id,mutation_kind,desired_json,status,created_at,updated_at) VALUES('deletion-outbox',987,?,'issue_comment','{}','succeeded','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')", issueID)
			exec("INSERT INTO runner_enrollments(id,organization_id,runner_id,machine_id,token_hash,operations_json,created_at,expires_at,created_by) VALUES('deletion-enrollment','org_browser_preview','shared-runner','deletion-machine',?,'[]','2026-01-01T00:00:00Z','2999-01-01T00:00:00Z',?)", strings.Repeat("a", 64), principal)
			exec("INSERT INTO runner_identities(id,organization_id,machine_id,token_id,enrollment_id,operations_json,created_at,capacity_limit,reported_capacity,last_heartbeat_at) VALUES('shared-runner','org_browser_preview','deletion-machine',?,'deletion-enrollment','[]','2026-01-01T00:00:00Z',1,1,'2026-01-01T00:00:00Z')", "shared-runner-token")
			exec("INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) SELECT 'deletion-change',organization_id,project_id,native_id,'{}' FROM issues WHERE id=?", issueID)
			exec("INSERT INTO change_versions(id,change_id,number,record_json) VALUES('deletion-version','deletion-change',1,'{}')")
			exec("INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,work_item_id) SELECT 'deletion-attachment',organization_id,project_id,?,'file.txt','text/plain',1,?,'2026-01-01T00:00:00Z',native_id FROM issues WHERE id=?", principal, strings.Repeat("b", 64), issueID)
			exec("INSERT INTO artifact_services(organization_id,project_id,id,binding_json,publisher_token_id) VALUES('org_browser_preview',?,'deletion-service','{}',?)", f.project, principal)
			exec("INSERT INTO artifact_references(organization_id,project_id,work_item_id,artifact_id,revision,service_id,manifest_id,reference_json) VALUES('org_browser_preview',?,?,'deletion-artifact',1,'deletion-service','manifest','{}')", f.project, item)
			for _, project := range []string{f.project, f.privateProject} {
				exec("INSERT INTO token_grants(token_id,organization_id,project_id) VALUES('shared-runner-token','org_browser_preview',?)", project)
				exec("INSERT INTO runner_enrollment_projects(enrollment_id,organization_id,project_id) VALUES('deletion-enrollment','org_browser_preview',?)", project)
				exec("INSERT INTO project_secrets(organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES('org_browser_preview',?,'fly_sprites_token','retained',X'01',X'02',X'03',1,'2026-01-01T00:00:00Z')", project)
			}
			exec("INSERT INTO project_sprite_pools(organization_id,project_id,configured_by) VALUES('org_browser_preview',?,?)", f.project, principal)
			exec("INSERT INTO project_sprite_members(organization_id,project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES('org_browser_preview',?,'sprite','org','deletion-enrollment','deleted','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')", f.project)
			if test.role != "" {
				var membership string
				if err := db.QueryRowContext(t.Context(), "SELECT membership_id FROM hosted_members WHERE user_id='user_browser_owner'").Scan(&membership); err != nil {
					t.Fatal(err)
				}
				if err := f.provider.SetMembershipRole(t.Context(), membership, test.role); err != nil {
					t.Fatal(err)
				}
				exec("UPDATE hosted_members SET role=? WHERE user_id='user_browser_owner'", test.role)
			}
			if test.update != "" {
				exec(test.update, f.project)
			}
			other := f.rawAPI(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/projects/"+f.privateProject+"/work-items", `{"idempotency_key":"other-project-item","title":"Other project active work","state":"Todo"}`, map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies["owner"].Value)})
			browserHostedStatus(t, other, http.StatusOK)
			exec("INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) SELECT 'other-workspace',organization_id,project_id,native_id,'branch','requested',300,'2999-01-01T00:00:00Z','2999-01-01T00:00:00Z',?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM issues WHERE project_id=?", principal, f.privateProject)
			before := deletionDataSnapshot(t, db)
			organization, confirm := test.organization, test.confirm
			if organization == "" {
				organization = "org_browser_preview"
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
			base := "/api/v2/organizations/" + organization + "/projects/" + f.project
			response := f.rawAPI(t, test.account, http.MethodDelete, base, string(body), headers)
			browserHostedStatus(t, response, test.status)
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
			for _, table := range []string{"issues", "native_attempts", "workspace_sessions", "project_action_runs", "conversations", "conversation_messages", "github_imports", "github_outbox", "project_sprite_members", "repositories", "machines", "runner_identities", "organizations", "attachments", "artifact_references", "change_requests", "change_versions"} {
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
				"SELECT count(*) FROM repositories":                                               1,
				"SELECT count(*) FROM github_outbox WHERE status='succeeded'":                     1,
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
	data := map[string][][]any{}
	for _, table := range []string{"projects", "issues", "native_attempts", "workspace_sessions", "project_action_runs", "conversations", "conversation_messages", "github_imports", "github_outbox", "project_secrets", "hosted_project_grants", "token_grants", "runner_enrollment_projects", "project_sprite_members", "repositories", "machines", "runner_identities", "organizations", "attachments", "artifact_references", "change_requests", "change_versions"} {
		rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table+" ORDER BY rowid")
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
			data[table] = append(data[table], values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
