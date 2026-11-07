package postgres_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthPageFixedWireBudget(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	counts := &pageWireCounts{}
	measured := pageMeasuredRepository(t, pool, counts)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
	}
	for _, size := range []int{2, 50} {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			_, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes)
SELECT $1,'finding_'||n,'finding_'||n,'signal','class','work_item','wi_first_1',now(),now(),'attention',$1,'inspect','','',0,0 FROM generate_series(1,$2::int) n ON CONFLICT DO NOTHING`, org, size)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) SELECT organization_id,id,'prj_first' FROM health_findings WHERE organization_id=$1 ON CONFLICT DO NOTHING`, org)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_issues SELECT organization_id,id,'prj_first','wi_first_1' FROM health_findings WHERE organization_id=$1 ON CONFLICT DO NOTHING`, org)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_detector_ticks VALUES($1,now()) ON CONFLICT DO NOTHING`, org)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(healthstore.Reader).HealthFindings(t.Context(), "prj_first", "all", nil, 0, "example", 100)
				if !errors.Is(err, healthstore.ErrPagePending) {
					t.Fatalf("cold projection should be pending: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for {
				var candidate healthstore.Finding
				err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
					var err error
					candidate, err = r.(healthstore.PageMaintainer).NextHealthPage(t.Context())
					return err
				})
				if errors.Is(err, corestore.ErrNotFound) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					_, err := w.(healthstore.PageMaintainer).PublishHealthPage(t.Context(), candidate, healthstore.Evidence{Reason: string(org), Counts: map[string]int{"attempts": 3}, Queues: map[string]healthstore.QueueEvidence{"prj_first": {QueueDepth: 4}}})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Run(fmt.Sprintf("%s/%dx", org, size), func(t *testing.T) {
				counts.reset()
				err := measured.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.(healthstore.Reader).HealthFindings(t.Context(), "prj_first", "open", nil, 0, "example", 2)
					if err == nil && (len(page.Findings) != 2 || page.Findings[0].Summary != string(org) || len(page.Findings[0].Issues) != 1 || page.Findings[0].DisplayEvidence.Reason != string(org) || page.Findings[0].DisplayEvidence.Counts["attempts"] != 3 || page.Findings[0].DisplayEvidence.Queues["prj_first"].QueueDepth != 4) {
						t.Fatalf("tenant page = %+v", page)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				statements, exchanges := counts.read()
				t.Logf("statements=%d exchanges=%d", statements, exchanges)
				if statements != 5 || exchanges != 4 {
					t.Errorf("want fixed 5 statements / 4 exchanges including transaction; got %d / %d", statements, exchanges)
				}
			})
		}
	}
}
