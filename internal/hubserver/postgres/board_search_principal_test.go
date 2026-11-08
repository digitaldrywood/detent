package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type principalSearchGateKey struct{}

type principalSearchGate struct {
	ready, release chan struct{}
	once           sync.Once
}

type principalSearchTracer struct{}

func (principalSearchTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if gate, ok := ctx.Value(principalSearchGateKey{}).(*principalSearchGate); ok && strings.Contains(query.SQL, "literal_positions") {
		gate.once.Do(func() {
			close(gate.ready)
			<-gate.release
		})
	}
	return ctx
}

func (principalSearchTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestBoardSearchPrincipalAdmission(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedBoardSearchProject(t, f, org, "prj_second", "another", []tracker.NativeWorkItemID{"wi_second_1"})
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE search_postings SET literal_positions=ARRAY(SELECT generate_series(1,80000,2)::bigint) WHERE document_id='issue/wi_first_1' AND cardinality(literal_positions)>0`); err != nil {
		t.Fatal(err)
	}
	config := f.pool.Config()
	config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, principalSearchTracer{})
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(ctx, pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for iteration := range 3 {
			t.Run(fmt.Sprintf("%s/%d", org, iteration), func(t *testing.T) {
				for _, tenant := range []tracker.OrganizationID{"org_first", "org_second"} {
					if _, err := f.admin.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("board-search:%q:%q", tenant, "example")); err != nil {
						t.Fatal(err)
					}
				}
				gate := &principalSearchGate{ready: make(chan struct{}), release: make(chan struct{})}
				finished := make(chan struct{})
				var hostileErr error
				t.Cleanup(func() {
					close(gate.release)
					<-finished
					var pgErr *pgconn.PgError
					timedOut := errors.Is(hostileErr, context.DeadlineExceeded) || (errors.As(hostileErr, &pgErr) && pgErr.Code == "57014")
					if !timedOut {
						t.Errorf("hostile search escaped deadline: %v", hostileErr)
					}
				})
				go func() {
					defer close(finished)
					hostile := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "cabcab", Limit: 1, Reporter: true, ReporterPrincipalID: "example"}
					hostileCtx := context.WithValue(ctx, principalSearchGateKey{}, gate)
					hostileErr = repository.ReadTenant(hostileCtx, org, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.BoardPage(hostileCtx, hostile)
						return err
					})
				}()
				select {
				case <-gate.ready:
				case <-finished:
					t.Fatalf("hostile search failed before acquiring admission: %v", hostileErr)
				}
				for _, model := range []bool{false, true} {
					read := func(tenant tracker.OrganizationID, selection viewstore.BoardSelection) error {
						return repository.ReadTenant(ctx, tenant, corestore.Serving, func(r tenantstore.Reader) error {
							var rows []viewstore.BoardRecord
							var err error
							if model {
								page, readErr := r.(interface {
									BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
								}).BoardModelPages(ctx, selection, nil, accessstore.Authority{})
								rows, err = page.Records, readErr
							} else {
								rows, err = r.BoardPage(ctx, selection)
							}
							if err == nil && (len(rows) != 1 || rows[0].OrganizationID != tenant || rows[0].ProjectID != selection.ProjectID || rows[0].PageIssue.Actor.PrincipalID != selection.ReporterPrincipalID) {
								return fmt.Errorf("search returned unexpected reporter rows: %+v", rows)
							}
							return err
						})
					}
					same := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "abc", Limit: 1, Reporter: true, ReporterPrincipalID: "example"}
					if err := read(org, same); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("model=%t same principal escaped concurrency bound: %v", model, err)
					}
					other := same
					other.ProjectID, other.ReporterPrincipalID = "prj_second", "another"
					if err := read(org, other); err != nil {
						t.Fatalf("model=%t second principal in same tenant starved: %v", model, err)
					}
					otherTenant := tracker.OrganizationID("org_first")
					if org == otherTenant {
						otherTenant = "org_second"
					}
					if err := read(otherTenant, same); err != nil {
						t.Fatalf("model=%t same principal in other tenant starved: %v", model, err)
					}
				}
			})
		}
	}
}
