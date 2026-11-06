package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubImportRecordsPageWithinProject(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	if err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org", Name: "Example", Slug: "example", Local: true, CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []tracker.ProjectID{"one", "two"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}); err != nil {
				return err
			}
			if _, err := w.(projectstore.GitHubImportWriter).CreateGitHubImport(t.Context(), projectstore.GitHubImport{OrganizationID: "org", ProjectID: project, ID: "same", IssueNumber: 1, Stage: "issue", Status: "pending", ObservedAt: time.Now()}); err != nil {
				return err
			}
			owner, ok := w.(projectstore.GitHubImportRecordWriter)
			if !ok {
				t.Fatal("typed GitHub import records owner unavailable")
			}
			for _, key := range []string{"first", "second"} {
				payload := eventstore.Receipt{OrganizationID: "org", PayloadObjectKey: "orgs/org/blobs/sha256/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadBytes: 10, PayloadContentBytes: 10}
				if err := owner.CreateGitHubImportRecord(t.Context(), projectstore.GitHubImportRecord{OrganizationID: "org", ProjectID: project, ImportID: "same", SourceKey: key, Kind: "comment", ProvenanceExternalID: key, Payload: payload}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err := repository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
		owner := r.(projectstore.GitHubImportRecordReader)
		for _, project := range []tracker.ProjectID{"one", "two"} {
			rows, err := owner.GitHubImportRecords(t.Context(), project, "same", 0, 1)
			if err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].ProjectID != project || rows[0].SourceKey != "first" {
				t.Fatalf("page=%+v", rows)
			}
			next, err := owner.GitHubImportEditSource(t.Context(), project, "same", rows[0].Sequence)
			if err != nil {
				return err
			}
			if next.SourceKey != "second" || next.ProjectID != project {
				t.Fatalf("next=%+v", next)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
