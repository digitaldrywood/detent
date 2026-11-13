package changerecords_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type priorApprovalCapture struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (c *priorApprovalCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "version_id IS NULL AND sequence>$8") {
		c.mu.Lock()
		c.sql, c.args = data.SQL, append([]any(nil), data.Args...)
		c.mu.Unlock()
	}
	return ctx
}

func (c *priorApprovalCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPriorApprovalCandidateSkipsCurrentVersionApprovals(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	capture := &priorApprovalCapture{}
	observed := pgstoretest.TracedFeaturePool(t, pool, capture)
	repository, err := postgres.NewRepository(t.Context(), observed, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	runtime, err := postgres.NewRuntime(t.Context(), observed, observed)
	if err != nil {
		t.Fatal(err)
	}
	const currentApprovals = 5000
	type prior struct {
		version  string
		sequence int64
	}
	for _, org := range []tracker.OrganizationID{"org_prior_first", "org_prior_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		for _, tc := range []struct {
			name  string
			prior []prior
		}{
			{name: "none"},
			{name: "lower", prior: []prior{{"older", 10}, {"older", 20}}},
			{name: "higher", prior: []prior{{"newer", 30}}},
			{name: "unversioned", prior: []prior{{"", 40}}},
			{name: "every_range", prior: []prior{{"", 5}, {"newer", 6}, {"older", 7}}},
		} {
			t.Run(string(org)+"/"+tc.name, func(t *testing.T) {
				ctx := t.Context()
				fixture := pgstoretest.SeedHistory(t, repository, org, "prior_"+tc.name)
				older := fixture.Version.ID
				versions := map[string]string{"older": older, "current": older + "_m", "newer": older + "_z"}
				for number, key := range []string{"current", "newer"} {
					version, err := fixture.Content.Version(ctx, fixture.Version)
					if err != nil {
						t.Fatal(err)
					}
					version.ID, version.Number = versions[key], int64(number+2)
					row, err := fixture.Content.PrepareVersion(ctx, org, pgstoretest.HistoryJSON(t, version))
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteRecords(ctx, org, corestore.Serving, func(w eventstore.Writer) error {
						_, err := w.CreateVersion(ctx, row)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				insert := `INSERT INTO change_evidence (quality_at,project_id,organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind)
SELECT $5::timestamptz,(SELECT project_id FROM change_requests WHERE organization_id=$1 AND id=$2),$1,$2,sequence,version_id,'review','human','example',decision,$5,$6,$7,1,1,review_kind FROM (`
				seeded := `SELECT 1000+n AS sequence,$3::text AS version_id,'approved' AS decision,CASE WHEN n%2=0 THEN 'human' ELSE '' END AS review_kind FROM generate_series(1,` + fmt.Sprint(currentApprovals) + `) n
 UNION ALL SELECT 10000+n,$4::text,'changes_requested','human' FROM generate_series(1,100) n
 UNION ALL SELECT 20000+n,$4::text,'approved','security_audit' FROM generate_series(1,100) n`
				if _, err := admin.ExecContext(ctx, insert+seeded+`) seeded`, org, fixture.Change.ID, versions["current"], older, pgstoretest.HistoryAt(), fixture.Evidence.PayloadObjectKey, fixture.Evidence.PayloadSHA256); err != nil {
					t.Fatal(err)
				}
				for _, row := range tc.prior {
					var version any
					if row.version != "" {
						version = versions[row.version]
					}
					if _, err := admin.ExecContext(ctx, insert+`SELECT $3::bigint AS sequence,$4::text AS version_id,'approved' AS decision,'' AS review_kind) seeded`, org, fixture.Change.ID, row.sequence, version, pgstoretest.HistoryAt(), fixture.Evidence.PayloadObjectKey, fixture.Evidence.PayloadSHA256); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := admin.ExecContext(ctx, "ANALYZE change_evidence"); err != nil {
					t.Fatal(err)
				}
				through := int64(30000)
				want := slices.Clone(tc.prior)
				slices.SortFunc(want, func(a, b prior) int {
					if (a.version == "") != (b.version == "") {
						if a.version == "" {
							return 1
						}
						return -1
					}
					if c := strings.Compare(versions[a.version], versions[b.version]); c != 0 {
						return c
					}
					return int(a.sequence - b.sequence)
				})
				var cursor eventstore.PriorApprovalCursor
				for step := 0; step <= len(want); step++ {
					var candidate eventstore.Evidence
					err := repository.ReadRecords(ctx, org, corestore.Serving, func(r eventstore.Reader) error {
						var err error
						candidate, err = r.(eventstore.CurrentChangeEvidenceReader).PriorApprovalCandidate(ctx, fixture.Change.ID, versions["current"], through, cursor)
						return err
					})
					if step == len(want) {
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("exhausted prior approvals returned %+v error=%v", candidate, err)
						}
					} else {
						if err != nil {
							t.Fatalf("prior approval %d: %v", step, err)
						}
						if candidate.OrganizationID != org || candidate.Sequence != want[step].sequence || hubVersion(candidate.VersionID) != versions[want[step].version] {
							t.Fatalf("prior approval %d = sequence %d version %q, want sequence %d version %q", step, candidate.Sequence, hubVersion(candidate.VersionID), want[step].sequence, versions[want[step].version])
						}
						cursor = eventstore.PriorApprovalCursorAfter(candidate)
					}
					capture.mu.Lock()
					statement, args := capture.sql, append([]any(nil), capture.args...)
					capture.mu.Unlock()
					tx, err := runtime.Begin(ctx, org, pgtransport.Options{ReadOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					var raw []byte
					err = tx.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&raw)
					_ = tx.Rollback(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var plans []struct{ Plan pgstoretest.SearchPlanNode }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("prior approval plan error=%v", err)
					}
					visits := float64(0)
					var inspect func(pgstoretest.SearchPlanNode)
					inspect = func(node pgstoretest.SearchPlanNode) {
						if node.Relation == "change_evidence" {
							visits += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
					t.Logf("prior approval probe %d visited %.0f evidence rows behind %d current-version approvals", step, visits, currentApprovals)
					if visits > 3 || !plans[0].Plan.UsesIndex("change_prior_approval_version_idx") {
						t.Fatalf("prior approval probe %d visited %.0f evidence rows behind %d current-version approvals: %s", step, visits, currentApprovals, raw)
					}
				}
			})
		}
	}
}

func hubVersion(version *string) string {
	if version == nil {
		return ""
	}
	return *version
}
