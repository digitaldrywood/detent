package postgres_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthPageCursorPreservesMatchesAcrossEmptyWindows(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: "wi_first_other", Number: 3, NativeSourceKey: new("source_other"), Title: "Example title", ActorKind: new("human"), ActorPrincipalID: new("other")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,resolved_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes,page_checkpoint)
 SELECT $1,'finding_'||n,'finding_'||n,'signal','class','project','prj_first',$2::timestamptz-interval '1 day',CASE WHEN n IN(1,2,8,9) THEN $2::timestamptz-interval '1 day' ELSE $2::timestamptz END,CASE WHEN n IN(1,8) THEN $2::timestamptz-interval '1 day' WHEN n IN(3,6) THEN $2::timestamptz END,'attention',$1,'inspect','example/evidence',repeat('a',64),1,1,1 FROM generate_series(1,9) n`, org, at); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) SELECT organization_id,id,CASE WHEN id='finding_7' THEN 'prj_second' ELSE 'prj_first' END FROM health_findings WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_finding_issues SELECT organization_id,finding_id,project_id,CASE WHEN finding_id='finding_4' THEN 'wi_first_other' WHEN project_id='prj_second' THEN 'wi_second_1' ELSE 'wi_first_1' END FROM health_finding_projects WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"ready", "cold"} {
			if mode == "cold" {
				if _, err := admin.ExecContext(t.Context(), `UPDATE health_finding_projects SET page_sequence=0 WHERE organization_id=$1;`, org); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE health_findings SET page_checkpoint=0 WHERE organization_id=$1`, org); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				name, state, reporter string
				since                 *time.Time
				want                  []string
			}{
				{"open_since", "open", "example", &at, []string{"finding_5"}},
				{"resolved_since", "resolved", "example", &at, []string{"finding_3", "finding_6"}},
				{"all_since", "all", "example", &at, []string{"finding_2", "finding_3", "finding_5", "finding_6", "finding_9"}},
				{"manager", "open", "", &at, []string{"finding_4", "finding_5"}},
				{"no_reporter_match", "all", "missing", nil, []string{}},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", org, mode, tc.name), func(t *testing.T) {
					got := []string{}
					after := int64(0)
					emptyContinuation := false
					for windows := 0; ; windows++ {
						if windows > 9 {
							t.Fatal("cursor failed to terminate")
						}
						var page healthstore.Page
						err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							page, err = r.(healthstore.Reader).HealthFindings(t.Context(), "prj_first", tc.state, tc.since, after, tc.reporter, 2)
							if mode == "cold" {
								if !errors.Is(err, healthstore.ErrPagePending) {
									t.Fatalf("cold page error=%v", err)
								}
								page, err = r.(healthstore.FallbackReader).LegacyHealthFindings(t.Context(), "prj_first", tc.state, tc.since, after, tc.reporter, 2)
							}
							return err
						})
						if err != nil {
							t.Fatal(err)
						}
						for _, f := range page.Findings {
							if f.Summary != string(org) || len(f.Issues) != 1 || f.Issues[0].ProjectID != "prj_first" || f.Evidence.Key != "example/evidence" || f.Evidence.Codec != "gzip" || f.Evidence.SchemaVersion != 1 || f.Evidence.Kind != "archive" || f.Evidence.Retention != "organization" {
								t.Fatalf("tenant/authorization/evidence changed: %+v", f)
							}
							got = append(got, f.ID)
						}
						if page.NextSequence == 0 {
							break
						}
						if page.NextSequence <= after {
							t.Fatalf("cursor did not advance: %d <= %d", page.NextSequence, after)
						}
						if len(page.Findings) == 0 {
							emptyContinuation = true
						}
						after = page.NextSequence
					}
					if !slices.Equal(got, tc.want) {
						t.Fatalf("matches=%v want=%v", got, tc.want)
					}
					if !emptyContinuation && tc.name != "all_since" {
						t.Fatal("fixture did not exercise an empty page with continuation")
					}
				})
			}
		}
	}
}
