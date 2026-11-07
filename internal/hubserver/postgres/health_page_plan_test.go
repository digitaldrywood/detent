package postgres_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthPageSparseFiltersBoundHistoryReads(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
	}
	for _, history := range []int{2000, 50000} {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,resolved_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes,page_checkpoint)
 SELECT $1,'history_'||n,'history_'||n,'signal','class','project','prj_first',$3::timestamptz-interval '1 day',$3::timestamptz-interval '1 day',$3::timestamptz-interval '1 day','attention',$1,'inspect','','',0,0,1 FROM generate_series(1,$2::int) n ON CONFLICT DO NOTHING`, org, history, at); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes,page_checkpoint)
 VALUES($1,$2,$2,'signal','class','project','prj_first',$3,$3,'attention',$1,'inspect','','',0,0,1)`, org, fmt.Sprintf("sparse_%d", history), at); err != nil {
				t.Fatal(err)
			}

			if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) SELECT organization_id,id,'prj_first' FROM health_findings WHERE organization_id=$1 ON CONFLICT DO NOTHING`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_finding_issues SELECT organization_id,id,'prj_first','wi_first_1' FROM health_findings WHERE organization_id=$1 ON CONFLICT DO NOTHING`, org); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), `ANALYZE health_findings; ANALYZE health_finding_projects; ANALYZE health_finding_issues; ANALYZE issues`); err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			var late int64
			if err := admin.QueryRowContext(t.Context(), `SELECT sequence FROM health_findings WHERE organization_id=$1 AND id=$2`, org, fmt.Sprintf("sparse_%d", history)).Scan(&late); err != nil {
				t.Fatal(err)
			}

			for _, tc := range []struct {
				name, state, reporter string
				since                 *time.Time
				after                 int64
				want                  string
			}{
				{"open", "open", "", nil, 0, ""}, {"time", "resolved", "", &at, 0, ""}, {"reporter", "all", "missing", nil, 0, ""}, {"combined", "open", "example", &at, 0, ""},
				{"sparse_tail", "open", "example", &at, late - 2, fmt.Sprintf("sparse_%d", history)},
			} {
				for _, mode := range []string{"ready", "fallback"} {
					t.Run(fmt.Sprintf("%s/%d/%s/%s", org, history, tc.name, mode), func(t *testing.T) {
						var statement string
						ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
							if strings.Contains(query, "selected AS MATERIALIZED") {
								statement = query
							}
						})
						if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
							var page healthstore.Page
							var err error
							if mode == "ready" {
								page, err = r.(healthstore.Reader).HealthFindings(ctx, "prj_first", tc.state, tc.since, tc.after, tc.reporter, 2)
							} else {
								page, err = r.(healthstore.FallbackReader).LegacyHealthFindings(ctx, "prj_first", tc.state, tc.since, tc.after, tc.reporter, 2)
							}
							if err == nil && tc.want == "" && page.NextSequence <= 0 {
								t.Fatal("empty page lost continuation")
							}
							wantCount := 0
							if tc.want != "" {
								wantCount = 1
							}
							if err == nil && (len(page.Findings) != wantCount || (wantCount == 1 && page.Findings[0].ID != tc.want)) {
								t.Fatalf("filtered page = %+v", page)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
						if statement == "" {
							t.Fatal("serving query not observed")
						}
						var raw []byte
						if err := admin.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, org, "prj_first", tc.state, tc.since, tc.after, tc.reporter, 2).Scan(&raw); err != nil {
							t.Fatal(err)
						}
						var plans []struct{ Plan searchPlanNode }
						if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
							t.Fatalf("decode plan: %v", err)
						}
						var visits func(searchPlanNode) float64
						visits = func(p searchPlanNode) float64 {
							n := (p.Rows + p.Removed + p.IndexRemoved) * p.Loops
							if p.Relation == "" {
								n = 0
							}
							for _, child := range p.Plans {
								n += visits(child)
							}
							return n
						}
						rows := visits(plans[0].Plan)
						t.Logf("history=%d limit=2 relation rows examined=%.0f", history, rows)
						if rows > 100 {
							t.Fatalf("fixed-size page examined %.0f relation rows", rows)
						}
					})
				}
			}
		}
	}
}
