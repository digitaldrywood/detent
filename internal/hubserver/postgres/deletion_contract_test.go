package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDeletionContractRefusesEachActiveFamilyIndependently(t *testing.T) {
	cases := []struct {
		name    string
		queries []string
	}{
		{"workspace", []string{`INSERT INTO workspace_sessions(organization_id,project_id,id,subject_work_item_id,ref,state,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) SELECT org,project,'ws_'||md5(project),item,'refs/heads/example','requested',ARRAY[]::text[],300,at+interval '1 hour',at+interval '1 minute','first',at,at FROM seed`}},
		{"action", []string{`INSERT INTO workspace_sessions(organization_id,project_id,id,subject_work_item_id,ref,state,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) SELECT org,project,'ws_'||md5(project),item,'refs/heads/example','closed',ARRAY[]::text[],300,at+interval '1 hour',at+interval '1 minute','first',at,at FROM seed`, `INSERT INTO project_actions(organization_id,project_id,id,slot,name,created_by,revision,created_at,updated_at,command_object_key,command_sha256,command_bytes,command_content_bytes) SELECT org,project,'action',0,'example','first',1,at,at,'example',repeat('a',64),1,1 FROM seed`, `INSERT INTO project_action_runs(organization_id,project_id,id,action_id,workspace_id,command_object_key,command_sha256,command_bytes,command_content_bytes,created_by,created_at,updated_at) SELECT org,project,'run','action','ws_'||md5(project),'example',repeat('a',64),1,1,'first',at,at FROM seed`}},
		{"conversation", []string{`INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,execution_status,execution_lease_id) SELECT org,project,project,'first','user','private','settled','running','' FROM seed`}},
		{"message", []string{`INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,execution_status,execution_lease_id) SELECT org,project,project,'first','user','private','settled','idle','' FROM seed`, `INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery) SELECT org,project,project,1,'user','text','queued' FROM seed`}},
		{"github-import", []string{`INSERT INTO github_imports(organization_id,project_id,id,issue_number,stage,status,observed_at) SELECT org,project,'import',1,'discover','pending',at FROM seed`}},
		{"github-outbound", []string{`INSERT INTO github_outbound(organization_id,project_id,work_item_id,command_key,repository,issue_number,kind,next_attempt_at,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT org,project,item,project,'example-org/example-project',1,'comment',at,at,'example',repeat('a',64),1,1 FROM seed`}},
		{"sprite", []string{`INSERT INTO runner_sprite_memberships(organization_id,name,provider_organization,enrollment_id,state,idle_since,created_at,allocation_project_id) SELECT org,project,'example','enrollment_'||replace(project,'prj_',''),'enrolled',at,at,project FROM seed`}},
		{"allocation", []string{`INSERT INTO organization_sprite_pools(organization_id,min_runners,max_runners,idle_seconds,configured_by,revision,isolation_tier,share_idle_capacity,placement_mode,todo_threshold,overflow_slots,max_sprite_slots) SELECT org,0,10,300,'first',1,'native-trusted',false,'blended',0,0,0 FROM seed ON CONFLICT DO NOTHING`, `INSERT INTO project_sprite_allocations(organization_id,project_id,desired_count,revision,provision_failures,requested_at,configured_by) SELECT org,project,1,1,0,at,'first' FROM seed`}},
		{"intake", []string{`INSERT INTO onboarding_issue_intake(organization_id,project_id,id,revision,batch_status,hub_owned,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT org,project,'batch',1,'discovering',true,'example',repeat('a',64),1,1 FROM seed`}},
		{"artifact-authorization", []string{`INSERT INTO artifact_services(organization_id,project_id,id,origin,binding_mode,hosted_opt_in,publisher_token_id) SELECT org,project,'service','https://example.invalid','customer',false,'first' FROM seed`, `INSERT INTO artifact_upload_authorizations(organization_id,project_id,service_id,idempotency_key,work_item_id,run_id,attempt_id,kind,bytes,created_at,expires_at) SELECT org,project,'service','upload',item,'run','attempt','log',1,at,clock_timestamp()+interval '1 hour' FROM seed`}},
		{"slack-delivery", []string{`INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) SELECT org,project,project,'signal','class','work_item',item,at,at,'attention','example','inspect','','',0,0 FROM seed`, `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) SELECT org,project,project FROM seed`, `INSERT INTO health_slack_deliveries(organization_id,id,finding_id,event,event_at,queued_at,next_attempt_at) SELECT org,project,project,'opened',at,at,at FROM seed`}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			admin := template.Database(t)
			repository, _ := repositoryFor(t, admin, smallPool())
			now := historyAt()
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedRepository(t, repository, org)
				for _, id := range []string{"first", "second"} {
					seedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
				}
				for _, id := range []string{"first", "second"} {
					project := tracker.ProjectID("prj_" + id)
					item := "wi_" + id + "_1"
					for _, query := range test.queries {
						if _, err := admin.ExecContext(t.Context(), `WITH seed AS (SELECT $1::text org,$2::text project,$3::text item,$4::timestamptz at) `+query, org, project, item, now); err != nil {
							t.Fatal(err)
						}
					}
					err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), project, now) })
					if !errors.Is(err, projectstore.ErrActiveWork) {
						t.Fatalf("tenant=%s project=%s active family=%s error=%v", org, project, test.name, err)
					}
					err = repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error { return w.DeleteProject(t.Context(), project) })
					if !errors.Is(err, projectstore.ErrActiveWork) {
						t.Fatalf("delete bypassed active family=%s error=%v", test.name, err)
					}
				}
				if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_idle", Name: "Idle", Profile: "native"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), "prj_idle", now) }); err != nil {
					t.Fatalf("other project activity leaked: %v", err)
				}
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			repository, _ = repositoryFor(t, admin, smallPool())
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), "prj_first", now) }); !errors.Is(err, projectstore.ErrActiveWork) {
					t.Fatalf("reopened activity=%v", err)
				}
				if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error { return w.DeleteProject(t.Context(), "prj_idle") }); err != nil {
					t.Fatalf("idle delete=%v", err)
				}
			}
		})
	}
}
