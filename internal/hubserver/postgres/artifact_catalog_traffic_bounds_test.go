package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type catalogTrafficStatement struct {
	sql  string
	args []any
}

type catalogTrafficTrace struct {
	mu         sync.Mutex
	statements []catalogTrafficStatement
}

func (p *catalogTrafficTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "artifact_catalog_traffic") || strings.Contains(query.SQL, "artifact_catalog_service_traffic") || strings.Contains(query.SQL, "-- name: CatalogTrafficPruneProjects") {
		p.mu.Lock()
		p.statements = append(p.statements, catalogTrafficStatement{sql: query.SQL, args: append([]any(nil), query.Args...)})
		p.mu.Unlock()
	}
	return ctx
}

func (*catalogTrafficTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *catalogTrafficTrace) take() []catalogTrafficStatement {
	p.mu.Lock()
	defer p.mu.Unlock()
	statements := p.statements
	p.statements = nil
	return statements
}

func TestArtifactCatalogTrafficPruneDoesNotWalkRevokedProjectHistory(t *testing.T) {
	admin, repository, pool, now := artifactCatalogFixture(t)
	before := now.Truncate(time.Minute)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for index := range 10 {
				project := tracker.ProjectID(fmt.Sprintf("prj_z_%02d", index))
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: fmt.Sprintf("page-%d", index), Profile: "native"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, ProjectID: project, TokenID: "first"}); err != nil {
					return err
				}
				for service := 1; service <= 2; service++ {
					if err := w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: fmt.Sprintf("service_%032x", service), Origin: "https://example.invalid", Mode: "customer", PublisherTokenID: "first"}); err != nil {
						return err
					}
				}
			}
			return w.DeleteAccessTokenGrant(t.Context(), "first", "prj_first")
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO artifact_catalog_traffic(organization_id,project_id,service_id,minute,upload_bytes,download_bytes,requests)
SELECT organization_id,project_id,id,$1::timestamptz - interval '60 days' + n * interval '1 minute',1,2,1 FROM artifact_services CROSS JOIN generate_series(1,10000) n WHERE project_id='prj_first'`,
		`INSERT INTO artifact_catalog_traffic_receipts(organization_id,project_id,service_id,id,minute,upload_bytes,download_bytes,requests)
SELECT organization_id,project_id,id,'revoked_'||n,$1::timestamptz - interval '60 days' + n * interval '1 minute',1,2,1 FROM artifact_services CROSS JOIN generate_series(1,10000) n WHERE project_id='prj_first'`,
		`INSERT INTO artifact_catalog_traffic(organization_id,project_id,service_id,minute,upload_bytes,download_bytes,requests)
SELECT organization_id,project_id,id,$1::timestamptz - interval '1 minute',1,2,1 FROM artifact_services WHERE project_id IN ('prj_second','prj_z_09') AND id='service_00000000000000000000000000000001'`,
		`INSERT INTO artifact_catalog_traffic_receipts(organization_id,project_id,service_id,id,minute,upload_bytes,download_bytes,requests)
SELECT organization_id,project_id,id,'eligible',$1::timestamptz - interval '1 minute',1,2,1 FROM artifact_services WHERE project_id IN ('prj_second','prj_z_09') AND id='service_00000000000000000000000000000001'`,
		`INSERT INTO artifact_catalog_service_traffic(organization_id,service_id,minute,upload_bytes,download_bytes,requests)
SELECT organization_id,service_id,minute,sum(upload_bytes),sum(download_bytes),sum(requests) FROM artifact_catalog_traffic WHERE minute<$1::timestamptz GROUP BY organization_id,service_id,minute`,
		`INSERT INTO artifact_catalog_service_traffic(organization_id,service_id,minute,upload_bytes,download_bytes,requests)
SELECT organization_id,'',minute,sum(upload_bytes),sum(download_bytes),sum(requests) FROM artifact_catalog_traffic WHERE minute<$1::timestamptz GROUP BY organization_id,minute`,
	} {
		if _, err := admin.ExecContext(t.Context(), statement, before); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), "ANALYZE artifact_catalog_traffic,artifact_catalog_traffic_receipts,artifact_catalog_service_traffic,artifact_services,token_grants"); err != nil {
		t.Fatal(err)
	}
	native := pool.Config()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	probe := &catalogTrafficTrace{}
	native.ConnConfig.Tracer = probe
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
		pool.Close()
	})
	const page = 7
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, test := range []struct {
			name, project string
			service, want int
		}{
			{name: "few-eligible", service: 1, want: 1},
			{name: "no-eligible", service: 2},
			{name: "revoked-project", project: "prj_first", service: 1},
			{name: "empty-authorized-project", project: "prj_second", service: 2},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				service := fmt.Sprintf("service_%032x", test.service)
				probe.take()
				var progress postgres.CatalogTrafficPruneProgress
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
					var err error
					progress, err = w.PruneScopedCatalogTraffic(t.Context(), postgres.ScopedCatalogTrafficPrune{OrganizationID: org, ServiceID: service, ProjectID: test.project, PublisherID: "first", Before: before, PageLimit: page})
					if err == nil && test.want > 0 {
						for _, lock := range []catalogTrafficStatement{
							{sql: "SELECT 1 FROM artifact_services WHERE organization_id=$1 AND project_id='prj_second' AND id=$2 FOR UPDATE NOWAIT", args: []any{org, service}},
							{sql: "SELECT 1 FROM token_grants WHERE organization_id=$1 AND project_id='prj_second' AND token_id='first' FOR UPDATE NOWAIT", args: []any{org}},
						} {
							verifyCatalogTrafficAuthorityLock(t, pool, org, lock)
						}
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				statements := probe.take()
				if len(statements) == 0 {
					t.Fatal("prune did not execute its traffic statements")
				}
				if progress.ServiceMinute.IsZero() || progress.OrganizationMinute.IsZero() {
					t.Fatal("protected summary prefix did not advance its inspected-candidate cursor", progress)
				}
				for _, table := range []string{"artifact_catalog_traffic", "artifact_catalog_traffic_receipts"} {
					var retained int
					if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE organization_id=$1 AND project_id='prj_first' AND service_id=$2", org, service).Scan(&retained); err != nil {
						t.Fatal(err)
					}
					if retained != 10000 {
						t.Fatalf("%s modified revoked-project history: retained=%d", table, retained)
					}
					if test.want > 0 {
						if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE organization_id=$1 AND project_id='prj_second' AND service_id=$2", org, service).Scan(&retained); err != nil {
							t.Fatal(err)
						}
						if retained != 0 {
							t.Fatalf("%s did not reach the authorized rows behind the revoked prefix: retained=%d", table, retained)
						}
					}
				}
				if test.project == "" {
					if progress.ProjectID == "" {
						t.Fatal("authorized project page did not return its seek position")
					}
					previous := progress
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
						var err error
						progress, err = w.PruneScopedCatalogTraffic(t.Context(), postgres.ScopedCatalogTrafficPrune{OrganizationID: org, ServiceID: service, PublisherID: "first", Before: before, PageLimit: page, AfterProject: previous.ProjectID, AfterServiceMinute: previous.ServiceMinute, AfterOrganizationMinute: previous.OrganizationMinute})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if !progress.ServiceMinute.After(previous.ServiceMinute) || !progress.OrganizationMinute.After(previous.OrganizationMinute) {
						t.Fatal("summary cleanup restarted at a protected prefix", previous, progress)
					}
					if test.want > 0 {
						var retained int
						if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM artifact_catalog_traffic WHERE organization_id=$1 AND project_id='prj_z_09' AND service_id=$2", org, service).Scan(&retained); err != nil {
							t.Fatal(err)
						}
						if retained != 0 {
							t.Fatal("empty authorized project prefix starved a later project", retained)
						}
					}
				}
				tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
					defer cancel()
					if err := tx.Rollback(ctx); err != nil {
						t.Error(err)
					}
				})
				if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(org)); err != nil {
					t.Fatal(err)
				}
				var visited, authorizationVisits float64
				for _, statement := range statements {
					previousAuthorizationVisits := authorizationVisits
					var raw []byte
					if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement.sql, statement.args...).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []struct{ Plan searchPlanNode }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatal("invalid PostgreSQL plan", err)
					}
					if strings.Contains(statement.sql, "-- name: PruneScopedCatalogTrafficReceipts") && !plans[0].Plan.usesIndex("artifact_catalog_traffic_receipts_project_expiry") {
						t.Fatalf("receipt pruning did not seek its project/minute index: %s", raw)
					}
					var inspect func(searchPlanNode)
					inspect = func(node searchPlanNode) {
						work := (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
						switch node.Relation {
						case "artifact_catalog_traffic", "artifact_catalog_traffic_receipts", "artifact_catalog_service_traffic":
							visited += work
						case "artifact_services", "token_grants":
							authorizationVisits += work
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
					t.Logf("%s: binding/grant visits=%.0f", strings.SplitN(statement.sql, "\n", 2)[0], authorizationVisits-previousAuthorizationVisits)
				}
				t.Logf("visited %.0f history rows plus %.0f binding/grant rows for page %d", visited, authorizationVisits, page)
				if visited+authorizationVisits > 16*page {
					t.Fatalf("prune inspected %.0f history and authorization rows for page %d behind a revoked prefix", visited+authorizationVisits, page)
				}
			})
		}
	}
}

func verifyCatalogTrafficAuthorityLock(t *testing.T, pool *pgxpool.Pool, org tracker.OrganizationID, statement catalogTrafficStatement) {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
		defer cancel()
		if err := tx.Rollback(ctx); err != nil {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(org)); err != nil {
		t.Fatal(err)
	}
	var present int
	err = tx.QueryRow(t.Context(), statement.sql, statement.args...).Scan(&present)
	var lockError *pgconn.PgError
	if !errors.As(err, &lockError) || lockError.Code != "55P03" {
		t.Fatal("traffic pruning did not retain its current binding/grant authority through commit", err)
	}
}
