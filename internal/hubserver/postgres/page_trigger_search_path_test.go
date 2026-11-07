package postgres_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPageTriggersSurviveRestrictedCallerSearchPath(t *testing.T) {
	admin, _ := migrationDatabaseAtVersion(t, 20261107062510)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO public.organizations(organization_id,name,local,created_at) VALUES($1,$1,false,now())`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO public.projects(organization_id,id,name,profile) VALUES($1,'prj_first','Example','native')`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO public.issues(organization_id,native_id,project_id,number,title) SELECT $1,'wi_first_'||n,'prj_first',n,'Example' FROM generate_series(1,2) n`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO public.health_findings
 (organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes)
 VALUES ($1,'finding_example','example','example','example','project','prj_first',now(),now(),'attention','Example','Example','example/evidence','example',1,1)`, org); err != nil {
			t.Fatal(err)
		}
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			for _, test := range []struct {
				name, statement, check string
			}{
				{"project update", `UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=$1 AND id='prj_first'`,
					`SELECT count(*) FROM public.project_integration_page_dirty WHERE organization_id=$1 AND project_id='prj_first'`},
				{"issue create", `INSERT INTO public.issues(organization_id,native_id,project_id,number,title) VALUES($1,'wi_example','prj_first',3,'Example')`,
					`SELECT count(*) FROM public.native_work_item_page_dirty WHERE organization_id=$1 AND work_item_id='wi_example' AND kind='item'`},
				{"search publication", `INSERT INTO public.search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 VALUES($1,'issue/wi_first_1','prj_first','wi_first_1','issue','wi_first_1',1,'Example','issue',now(),'example/payload',repeat('a',64),1,1)`,
					`SELECT count(*) FROM public.project_integration_page_dirty WHERE organization_id=$1 AND project_id='prj_first'`},
				{"onboarding invalidation", `INSERT INTO public.projects(organization_id,id,name,profile) VALUES($1,'prj_example','Another example','native')`,
					`SELECT count(*) FROM public.project_onboarding_page_access WHERE organization_id=$1`},
				{"relationship invalidation", `INSERT INTO public.issue_dependencies(organization_id,dependent_issue_id,blocker_issue_id,dependent_project_id,blocker_project_id)
 SELECT $1,a.id,b.id,a.project_id,b.project_id FROM public.issues a,public.issues b WHERE a.organization_id=$1 AND b.organization_id=$1 AND a.native_id='wi_first_1' AND b.native_id='wi_first_2'`,
					`SELECT count(*) FROM public.native_work_item_page_dirty WHERE organization_id=$1 AND work_item_id='wi_first_1' AND kind='relationships'`},
				{"health ordering", `INSERT INTO public.health_finding_projects(organization_id,finding_id,project_id) VALUES($1,'finding_example','prj_first')`,
					`SELECT count(*) FROM public.health_finding_projects p JOIN public.health_findings f ON (f.organization_id,f.id)=(p.organization_id,p.finding_id) WHERE p.organization_id=$1 AND p.page_sequence=f.sequence`},
			} {
				t.Run(test.name, func(t *testing.T) {
					tx, err := admin.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
							t.Error(err)
						}
					}()
					if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_runtime_v2`); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), `DELETE FROM public.project_integration_page_dirty WHERE organization_id=$1`, org); err != nil {
						t.Fatal(err)
					}
					pathSetting := "pg_catalog"
					if test.name == "issue create" || test.name == "onboarding invalidation" {
						pathSetting = "pg_catalog, public"
					}
					var accessGeneration int64
					if test.name == "onboarding invalidation" {
						if err := tx.QueryRowContext(t.Context(), `SELECT generation FROM public.project_onboarding_page_access WHERE organization_id=$1`, org).Scan(&accessGeneration); err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(t.Context(), `CREATE TEMP TABLE project_onboarding_page_access (LIKE public.project_onboarding_page_access INCLUDING ALL) ON COMMIT DROP`); err != nil {
							t.Fatal(err)
						}
					}
					if test.name == "health ordering" {
						if _, err := tx.ExecContext(t.Context(), `RESET ROLE`); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := tx.ExecContext(t.Context(), `SELECT set_config('search_path',$1,true)`, pathSetting); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), test.statement, org); err != nil {
						t.Fatal(err)
					}
					var count int
					if err := tx.QueryRowContext(t.Context(), test.check, org).Scan(&count); err != nil || count != 1 {
						t.Fatalf("trigger effect count=%d, want 1: %v", count, err)
					}
					if test.name == "onboarding invalidation" {
						var generation int64
						if err := tx.QueryRowContext(t.Context(), `SELECT generation FROM public.project_onboarding_page_access WHERE organization_id=$1`, org).Scan(&generation); err != nil || generation <= accessGeneration {
							t.Fatalf("onboarding generation=%d, before=%d: %v", generation, accessGeneration, err)
						}
						if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_temp.project_onboarding_page_access`).Scan(&count); err != nil || count != 0 {
							t.Fatalf("trigger wrote %d temporary rows: %v", count, err)
						}
					}
					var path string
					if err := tx.QueryRowContext(t.Context(), `SHOW search_path`).Scan(&path); err != nil || path != pathSetting {
						t.Fatalf("caller search path=%q: %v", path, err)
					}
				})
			}
		})
	}
}
