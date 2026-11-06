package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthFindingsTenantVisibilityAndCursor(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	at := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			_, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) VALUES($1,$2,$2,'signal','class','work_item','wi_first_1',$3,$3,'attention',$1,'inspect','','',0,0)`, org, id, at)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) VALUES($1,$2,'prj_first')`, org, id)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_issues(organization_id,finding_id,project_id,work_item_id) VALUES($1,$2,'prj_first','wi_first_1')`, org, id)
			if err != nil {
				t.Fatal(err)
			}
		}
		err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(healthstore.Reader)
			if !ok {
				t.Fatal("PostgreSQL health findings owner is missing")
			}
			page, err := owner.HealthFindings(t.Context(), "prj_first", "open", &at, 0, "example", 1)
			if err != nil {
				return err
			}
			if len(page.Findings) != 1 || page.Findings[0].Summary != string(org) || len(page.Findings[0].Issues) != 1 {
				t.Fatalf("first page = %+v", page)
			}
			next, err := owner.HealthFindings(t.Context(), "prj_first", "open", &at, page.Findings[0].Sequence, "example", 2)
			if err != nil {
				return err
			}
			if len(next.Findings) != 1 || next.Findings[0].ID == page.Findings[0].ID {
				t.Fatalf("next page = %+v", next)
			}
			for _, tc := range []struct {
				project  tracker.ProjectID
				reporter string
				since    time.Time
			}{{"prj_second", "example", at}, {"prj_first", "other", at}, {"prj_first", "example", at.Add(time.Microsecond)}} {
				hidden, err := owner.HealthFindings(t.Context(), tc.project, "open", &tc.since, 0, tc.reporter, 2)
				if err != nil {
					return err
				}
				if len(hidden.Findings) != 0 {
					t.Fatalf("hidden findings = %+v", hidden)
				}
			}
			summary, err := owner.HealthSummaryFindings(t.Context(), 100)
			if err != nil {
				return err
			}
			if len(summary) != 2 || len(summary[0].Projects) != 1 {
				t.Fatalf("summary = %+v", summary)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
