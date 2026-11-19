package searchrecords_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardViewReturnsTenantLinksInOrder(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
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
			})
		}
	}
}
