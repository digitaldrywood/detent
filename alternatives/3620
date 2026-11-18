package searchrecords_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
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
	if gate, ok := ctx.Value(principalSearchGateKey{}).(*principalSearchGate); ok && strings.Contains(query.SQL, "terms @@") {
		gate.once.Do(func() {
			close(gate.ready)
			<-gate.release
			<-ctx.Done()
		})
	}
	return ctx
}

func (principalSearchTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type firedDeadline struct {
	context.Context
	done chan struct{}
}

func (d *firedDeadline) Done() <-chan struct{} { return d.done }

func (d *firedDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestBoardSearchPrincipalAdmission(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedBoardSearchProject(t, f, org, "prj_second", "another", []tracker.NativeWorkItemID{"wi_second_1"})
	}
	config := f.pool.Config()
	config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, principalSearchTracer{})
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(ctx, pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1, SearchBudget: time.Hour})
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
				if _, err := f.admin.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("board-search:%q:%q", org, "example")); err != nil {
					t.Fatal(err)
				}
				gate := &principalSearchGate{ready: make(chan struct{}), release: make(chan struct{})}
				deadline := &firedDeadline{Context: context.WithValue(ctx, principalSearchGateKey{}, gate), done: make(chan struct{})}
				finished := make(chan struct{})
				var hostileErr error
				t.Cleanup(func() {
					close(deadline.done)
					close(gate.release)
					<-finished
					var pgErr *pgconn.PgError
					timedOut := errors.Is(hostileErr, context.DeadlineExceeded) || (errors.As(hostileErr, &pgErr) && pgErr.Code == "57014")
					if !timedOut || errors.Is(hostileErr, searchstore.ErrBusy) {
						t.Errorf("hostile search escaped its caller's deadline or was reported as a busy search: %v", hostileErr)
					}
				})
				go func() {
					defer close(finished)
					hostile := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "cabcab", Limit: 1, Reporter: true, ReporterPrincipalID: "example"}
					hostileErr = repository.ReadTenant(deadline, org, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.BoardPage(deadline, hostile)
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
					same := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "example", Limit: 1, Reporter: true, ReporterPrincipalID: "example"}
					if err := read(org, same); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, searchstore.ErrBusy) {
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
