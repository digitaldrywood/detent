package postgres_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptDiffBodyReferencesPreserveUnknownHistory(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, organization)
		ref, _, err := attachment.PrepareArchive(string(organization), 1, strings.NewReader("[]"))
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 2; index++ {
			err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				archive, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: "archive_example", Reference: ref, CreatedAt: historyAt()})
				if err != nil {
					return err
				}
				item, err := w.WorkItem(t.Context(), "prj_first", tracker.NativeWorkItemID(fmt.Sprintf("wi_first_%d", index+1)))
				if err != nil {
					return err
				}
				return w.CreateAttemptDiff(t.Context(), leasestore.AttemptDiff{OrganizationID: organization, ID: fmt.Sprintf("diff_%d", index), AttemptID: fmt.Sprintf("attempt_%d", index), IssueID: item.ID, ProjectID: "prj_first", WorkItemID: item.NativeID, Generation: tracker.DiffGeneration{Source: "attempt", Seq: 1}, Producer: tracker.DiffProducer{Kind: "attempt", ID: "attempt", RunnerID: "runner", LeaseID: "lease", FencingToken: 1}, ArchiveID: archive.ID, CreatedAt: historyAt()})
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		assertReference := func(project tracker.ProjectID, key string, wantReferenced, wantComplete bool) {
			t.Helper()
			err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				referenced, complete, err := r.AttemptDiffBodyReferenced(t.Context(), project, key)
				if err == nil && (referenced != wantReferenced || complete != wantComplete) {
					t.Errorf("reference=%v complete=%v, want %v %v", referenced, complete, wantReferenced, wantComplete)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		missing := diffbody.New(string(organization), "prj_first", "diff_missing", 0, "missing").Key
		assertReference("prj_first", missing, false, false)

		for _, test := range []struct{ name, key string }{
			{"empty", ""},
			{"foreign tenant", diffbody.New("org_other", "prj_first", "diff_example", 0, "example").Key},
			{"foreign project", diffbody.New(string(organization), "prj_second", "diff_example", 0, "example").Key},
			{"invalid digest", "orgs/" + string(organization) + "/attempt-diffs/prj_first/diff_example/0/invalid"},
			{"invalid position", diffbody.New(string(organization), "prj_first", "diff_example", -1, "example").Key},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					return w.IndexAttemptDiffBodies(t.Context(), "prj_first", "diff_0", []string{test.key})
				})
				if !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("invalid body key error=%v", err)
				}
			})
		}
		assertReference("prj_first", missing, false, false)
		err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.IndexAttemptDiffBodies(t.Context(), "prj_second", "diff_0", nil)
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("wrong project index error=%v", err)
		}
		for index := 0; index < 2; index++ {
			key := diffbody.New(string(organization), "prj_first", fmt.Sprintf("diff_external_%d", index), 0, "example").Key
			err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.IndexAttemptDiffBodies(t.Context(), "prj_first", fmt.Sprintf("diff_%d", index), []string{key})
			})
			if err != nil {
				t.Fatal(err)
			}
			assertReference("prj_first", key, true, index == 1)
			assertReference("prj_second", key, false, true)
		}
		assertReference("prj_first", missing, false, true)
		assertReference("prj_first", diffbody.New("org_other", "prj_first", "diff_external_0", 0, "example").Key, false, true)
		if _, err := admin.ExecContext(t.Context(), "UPDATE attempt_diffs SET manifest_expired=true WHERE organization_id=$1 AND id=$2", organization, "diff_0"); err != nil {
			t.Fatal(err)
		}
		assertReference("prj_first", diffbody.New(string(organization), "prj_first", "diff_external_0", 0, "example").Key, false, true)
	}
}
