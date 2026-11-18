package searchrecords_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const boardViewArrayRowElementBound = 256

type boardViewStatements struct {
	mu       sync.Mutex
	captured map[bool][]any
}

func (s *boardViewStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	for _, reporter := range []bool{false, true} {
		if data.SQL == boardviewsql.Statement(reporter) {
			s.mu.Lock()
			s.captured[reporter] = append([]any(nil), data.Args...)
			s.mu.Unlock()
		}
	}
	return ctx
}

func (s *boardViewStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *boardViewStatements) args(reporter bool) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captured[reporter]
}

func TestBoardViewReturnsLinksInBoundedRows(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	statements := &boardViewStatements{captured: map[bool][]any{}}
	traced := pgstoretest.TracedFeaturePool(t, pool, statements)
	repository, err := postgres.NewRepository(t.Context(), traced, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	content := viewstore.NewContent(pgstoretest.NewHistoryObjects())
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	const cards = 40
	const links = cards * (cards - 1)
	title := strings.Repeat("Linked example issue ", 4)
	id := func(index int) tracker.NativeWorkItemID {
		return tracker.NativeWorkItemID(fmt.Sprintf("wi_linked_%03d", index))
	}
	url := func(org tracker.OrganizationID, index int) string {
		return fmt.Sprintf("/%s/example/issues/%d", org, index+1)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}}); err != nil {
				return err
			}
			for index := range cards {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_example", NativeID: id(index), Number: int64(index + 1), Title: title, ActorKind: new("human"), ActorPrincipalID: new("first"), WorkflowStateName: new("Todo")}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for index := range cards {
			related := make([]tracker.NativeDependency, 0, cards-1)
			for other := range cards {
				if other != index {
					related = append(related, tracker.NativeDependency{ID: id(other), ProjectID: "prj_example", Number: other + 1, Title: title, State: "Todo", Dispatchable: true, Identifier: fmt.Sprintf("Example#%d", other+1), WebURL: url(org, other)})
				}
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_example", WorkItemID: id(index), Number: index + 1, Revision: 1}, Title: title, State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "first"}, Related: related, DependencyReady: true, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}
			payload, err := content.PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			record := viewstore.BoardRecord{OrganizationID: org, ProjectID: "prj_example", WorkItemID: id(index), SourceRevision: 1, Position: viewstore.BoardPosition{Lane: 1, State: "Todo", Activity: &now, Identifier: fmt.Sprintf("Example#%d", index+1)}, Dispatchable: true, ActorPrincipalID: "first", CreatedAt: now, UpdatedAt: now, Payload: payload,
				PageIssue: &issue, ListIssue: &issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{Related: related, DependencyReady: true}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: title, Number: index + 1, Actor: issue.Actor}}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "prj_example", WorkItemID: id(index), ExpectedRevision: 1, Current: &record})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, read := range []struct {
			name      string
			selection viewstore.BoardViewSelection
		}{
			{name: "another member", selection: viewstore.BoardViewSelection{PrincipalID: "second", UserID: "second", CredentialID: "second"}},
			{name: "reporter", selection: viewstore.BoardViewSelection{Reporter: "first", PrincipalID: "first", UserID: "first", CredentialID: "first"}},
		} {
			t.Run(string(org)+"/"+read.name, func(t *testing.T) {
				selection := read.selection
				selection.Projects, selection.Allowed, selection.ClosedPage, selection.LanePreset = []tracker.ProjectID{"prj_example"}, []tracker.ProjectID{"prj_example"}, 1, viewstore.LanesOpen
				var rows viewstore.BoardViewRows
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					model, ok := r.(interface {
						BoardView(context.Context, viewstore.BoardViewSelection) (viewstore.BoardViewRows, error)
					})
					if !ok {
						t.Fatal("tenant reader has no board view")
					}
					var err error
					rows, err = model.BoardView(t.Context(), selection)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				counts := map[string]int{}
				seen := map[string]bool{}
				for _, row := range rows.Rows {
					counts[row.Kind]++
					if row.Kind == "link" {
						seen[fmt.Sprintf("%s>%s#%d@%s", *row.ID, *row.Text[1], *row.Int[1], *row.Text[5])] = true
					}
				}
				if counts["card"] != cards || counts["link"] != links || counts["reason"] != cards {
					t.Fatalf("board read cards=%d links=%d reasons=%d, want %d, %d and %d", counts["card"], counts["link"], counts["reason"], cards, links, cards)
				}
				for index := range cards {
					ordinal := 0
					for other := range cards {
						if other == index {
							continue
						}
						ordinal++
						if link := fmt.Sprintf("%s>%s#%d@%s", id(index), id(other), ordinal, url(org, other)); !seen[link] {
							t.Fatalf("board read is missing link %s", link)
						}
					}
				}
				args := statements.args(selection.Reporter != "")
				if args == nil {
					t.Fatal("the board statement was not traced")
				}
				aggregated := boardViewAggregatedRows(t, traced, org, boardviewsql.Statement(selection.Reporter != ""), args)
				total := 0
				for _, row := range aggregated {
					if row.elements > boardViewArrayRowElementBound {
						t.Errorf("one %s row carries %d elements, over the %d element bound", row.kind, row.elements, boardViewArrayRowElementBound)
					}
					if row.kind == "links" {
						total += row.elements
					}
				}
				if total != links {
					t.Fatalf("aggregated link elements=%d want %d", total, links)
				}
			})
		}
	}
}

func TestBoardPreparedStatementReuse(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
	runtime := pgstoretest.RuntimeFor(t, pool, pool)
	tx, err := runtime.Begin(t.Context(), "org_example", pgtransport.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	query := boardviewsql.Statement(false)
	args := []any{"org_example", []string{"prj_example"}, []string{"prj_example"}, "", false, "credential", "principal", "member", 2, []string{}, false, viewstore.LanesOpen, nil, "", false, []string{}, []string{}}
	var first time.Time
	for i := 1; i <= 2; i++ {
		rows, err := tx.Query(t.Context(), query, args...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := boardviewsql.Scan(rows); err != nil {
			t.Fatal(err)
		}
		var prepared time.Time
		var uses int
		if err := tx.QueryRow(t.Context(), `SELECT prepare_time,generic_plans+custom_plans FROM pg_prepared_statements WHERE statement=$1`, query).Scan(&prepared, &uses); err != nil {
			t.Fatal(err)
		}
		if uses != i || i > 1 && !first.Equal(prepared) {
			t.Fatalf("board execution %d used plan prepared at %s (%d uses, first prepared at %s)", i, prepared, uses, first)
		}
		first = prepared
	}
}

type boardViewAggregatedRow struct {
	kind     string
	elements int
}

func boardViewAggregatedRows(t *testing.T, pool *pgxpool.Pool, org tracker.OrganizationID, statement string, args []any) []boardViewAggregatedRow {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", string(org)); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []boardViewAggregatedRow
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		kind, _ := values[0].(string)
		first, _ := values[len(values)-15].([]any)
		if first != nil {
			result = append(result, boardViewAggregatedRow{kind: kind, elements: len(first)})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
