package postgres_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimPreviewPlanDistinguishesProvenanceFromLinkedSources(t *testing.T) {
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	organization := tracker.OrganizationID("org_example")
	project := tracker.ProjectID("prj_example")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := pgstoretest.SeedCoreOrganization(t.Context(), w, organization); err != nil {
			return err
		}
		_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Profile: "native"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	for index, test := range []struct {
		name      string
		sourceKey string
		linked    bool
		wantError bool
	}{
		{"native", "", false, false},
		{"import provenance", "github:external_example", false, false},
		{"progress check", "progress-check:example", false, false},
		{"missing linked source", "github:https://github.com/example-org/example-project/issues/4", false, true},
		{"present linked source", "github:https://github.com/example-org/example-project/issues/5", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_example_%d", index))
			version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			var item corestore.WorkItem
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: id, Number: int64(index + 1), Title: "Example", NativeSourceKey: new(test.sourceKey)})
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
					return err
				}
				if test.linked {
					_, err = w.CreateLinkedSource(t.Context(), eventstore.LinkedSource{OrganizationID: organization, ProjectID: project, WorkItemID: id, URL: "https://github.com/example-org/example-project/issues/5"})
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.(tenantstore.ClaimPreviewPlanRowsReader).ClaimPreviewPlanRows(t.Context(), project, []claimstore.Candidate{{OrganizationID: organization, ProjectID: project, WorkItemID: id, ID: item.ID, Revision: item.Revision}})
				if err != nil {
					return err
				}
				if len(rows) != 1 {
					t.Fatalf("claim preview rows=%d, want 1", len(rows))
				}
				if test.wantError {
					if !errors.Is(rows[0].Err, corestore.ErrNotFound) {
						t.Errorf("missing linked source error=%v, want not found", rows[0].Err)
					}
				} else if rows[0].Err != nil {
					t.Errorf("claim preview error=%v", rows[0].Err)
				}
				if got := rows[0].LinkedSource != nil; got != test.linked {
					t.Errorf("linked source present=%t, want %t", got, test.linked)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
