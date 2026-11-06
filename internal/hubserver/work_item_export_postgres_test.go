package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresCanonicalExportSelection(t *testing.T) {
	repository, f, owner, expected, _ := newPostgresExportLinkedFixture(t)
	var principal string
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(t.Context(), owner.identity.Subject)
		principal = member.PrincipalID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		selection corestore.WorkItemExportSelection
		count     int64
	}{
		{"fresh", corestore.WorkItemExportSelection{}, 2},
		{"state", corestore.WorkItemExportSelection{States: []string{"Todo"}}, 2},
		{"other state", corestore.WorkItemExportSelection{States: []string{"Done"}}, 0},
		{"label", corestore.WorkItemExportSelection{Labels: []string{"missing"}}, 0},
		{"archive", corestore.WorkItemExportSelection{Archived: new(true)}, 0},
		{"open", corestore.WorkItemExportSelection{Terminal: new(false)}, 2},
		{"reporter", corestore.WorkItemExportSelection{Reporter: true, PrincipalID: principal}, 2},
		{"anonymous reporter", corestore.WorkItemExportSelection{Reporter: true}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.selection.ProjectID = f.project
			test.selection.IncludeTotal = true
			after, count := int64(0), int64(0)
			for {
				var rows []corestore.WorkItem
				var total int64
				err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					rows, total, err = r.ExportWorkItems(t.Context(), test.selection, after, 1)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if total != test.count {
					t.Fatalf("total=%d want=%d", total, test.count)
				}
				if len(rows) == 0 {
					break
				}
				if len(rows) != 1 || rows[0].Number <= after || expected[rows[0].NativeID] != rows[0].Title {
					t.Fatalf("invalid export page: %+v", rows)
				}
				after = rows[0].Number
				count++
				if count > test.count {
					t.Fatal("export page repeated rows")
				}
			}
			if count != test.count {
				t.Fatalf("rows=%d want=%d", count, test.count)
			}
		})
	}
}
