package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRelationshipReadModelHasFixedPageCost(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	counts := new(pageWireCounts)
	measured := pageMeasuredRepository(t, pool, counts)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)
		for _, size := range []int{2, 50} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for index := range size {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_page_%d_%d", size, index))
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_second", NativeID: id, Number: int64(size*100 + index), Title: string(org) + " target"})
					if err != nil {
						return err
					}
					if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", RelatedProjectID: "prj_second", RelatedWorkItemID: id, Type: "dependency"}, "add", historyAt()); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			projected := false
			for attempts := 0; attempts < 500; attempts++ {
				var candidate viewstore.PageCandidate
				var source corestore.RelationshipPage
				if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
					var err error
					candidate, err = r.(viewstore.WorkItemPageReader).NextWorkItemPageCandidate(t.Context())
					if err != nil {
						return err
					}
					if candidate.Kind == "relationships" && candidate.WorkItemID == "wi_first_1" {
						source, err = r.Relationships(t.Context(), candidate.ProjectID, candidate.WorkItemID, candidate.After, corestore.MaxPageSize)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					if err := w.(viewstore.WorkItemPageWriter).AdvanceWorkItemPageCursor(t.Context(), candidate); err != nil {
						return err
					}
					if candidate.Kind != "relationships" || candidate.WorkItemID != "wi_first_1" {
						return nil
					}
					records := []viewstore.RelationshipPageRecord{}
					for _, relation := range source.Items {
						records = append(records, viewstore.RelationshipPageRecord{Relationship: relation, Identifier: string(relation.RelatedProjectID) + "#" + fmt.Sprint(relation.Number), WebURL: "https://example.test/issue"})
					}
					projected = true
					return w.(viewstore.RelationshipPageWriter).PublishRelationshipPage(t.Context(), candidate, records, source.Next)
				}); err != nil {
					t.Fatal(err)
				}
				if projected {
					break
				}
			}
			if !projected {
				t.Fatal("relationship projection did not reach fixture source")
			}
			var retired int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_relationship_pages p JOIN native_relationship_page_heads h USING(organization_id,project_id,work_item_id) WHERE p.organization_id=$1 AND p.generation<>h.generation`, org).Scan(&retired); err != nil {
				t.Fatal(err)
			}
			if retired != 0 {
				t.Errorf("obsolete small relationship generations=%d, want zero", retired)
			}
			for _, limit := range []int{1, 2} {
				counts.reset()
				if err := measured.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.(viewstore.RelationshipPageReader).RelationshipPageRecords(t.Context(), "prj_first", "wi_first_1", corestore.RelationshipCursor{}, limit)
					if err != nil {
						return err
					}
					want := limit + 1
					if size == 2 && limit == 2 {
						want = 2
					}
					if len(rows) != want {
						t.Fatalf("size=%d limit=%d rows=%d", size, limit, len(rows))
					}
					for _, row := range rows {
						if row.Relationship.Direction != "blocked_by" || row.Relationship.Title != string(org)+" target" {
							t.Fatalf("direction or tenant leak: %+v", row)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				statements, exchanges := counts.read()
				checkoutSQL, checkoutRTT := counts.checkout()
				if checkoutSQL != 1 || checkoutRTT != 1 {
					t.Fatalf("forced pool ping: SQL=%d exchanges=%d, want 1/1", checkoutSQL, checkoutRTT)
				}
				t.Logf("tenant=%s scale=%d page=%d statements=%d exchanges=%d checkout_ping=1/1", org, size, limit, statements, exchanges)
				if statements != 4 || exchanges != 3 {
					t.Fatalf("statements=%d exchanges=%d, want 4/3", statements, exchanges)
				}
			}
			if size == 50 {
				id := "wi_page_2_0"
				if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='new_owner' WHERE organization_id=$1 AND project_id='prj_second' AND native_id=$2`, org, id); err != nil {
					t.Fatal(err)
				}
				check := func(deleted bool) {
					t.Helper()
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.(viewstore.RelationshipPageReader).RelationshipPageRecords(context.Background(), "prj_first", "wi_first_1", corestore.RelationshipCursor{}, 100)
						if err != nil {
							return err
						}
						for _, row := range rows {
							if string(row.Relationship.RelatedWorkItemID) == id {
								if !deleted && (row.Relationship.ActorPrincipalID == nil || *row.Relationship.ActorPrincipalID != "new_owner") {
									t.Error("projected target ownership remained stale")
								}
								if deleted && row.Relationship.Title != "" {
									t.Error("deleted target still discloses projected title")
								}
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				check(false)
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM issue_dependencies WHERE organization_id=$1 AND blocker_issue_id=(SELECT id FROM issues WHERE organization_id=$1 AND native_id=$2)`, org, id); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM issues WHERE organization_id=$1 AND project_id='prj_second' AND native_id=$2`, org, id); err != nil {
					t.Fatal(err)
				}
				check(true)
			}

		}
	}
}
