package postgres_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactReferencesAreImmutableAndTenantScoped(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	organizations := make([]tracker.OrganizationID, 2)
	references := make([]artifact.Reference, len(organizations))
	for index := range organizations {
		suffix := fmt.Sprintf("%032x", index+1)
		organization := tracker.OrganizationID("org_" + suffix)
		project := tracker.ProjectID("prj_" + fmt.Sprintf("%032x", index+101))
		item := tracker.NativeWorkItemID("wi_" + fmt.Sprintf("%032x", index+201))
		organizations[index] = organization
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native", WorkflowSource: "example", WorkflowSourceRevision: "revision", WorkflowObjectKey: new("example/workflow"), StatesObjectKey: new("example/states"), ModelSelectionObjectKey: new("example/models")}); err != nil {
				return err
			}
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, NativeID: item, ProjectID: project, Number: 1, NativeSourceKey: new("source_" + suffix), Title: "Example item", BodyObjectKey: new("example/body"), IssueContractObjectKey: new("example/contract"), ReportContextObjectKey: new("example/report"), ActorKind: new("human"), ActorPrincipalID: new("example"), ProvenanceProvider: new("example"), ProvenanceExternalID: new("external")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		references[index] = artifact.Reference{
			SchemaVersion: 1,
			Scope:         artifact.Scope{OrganizationID: string(organization), ProjectID: string(project), WorkItemID: string(item), RunID: "run_" + suffix, AttemptID: "attempt_" + suffix},
			ServiceID:     "service_" + suffix, ArtifactID: "artifact_" + suffix, ManifestID: "manifest_" + suffix, Revision: 1,
			SHA256: artifact.Digest([]byte(suffix)), Kind: "diff", State: "complete", Availability: "available",
			Bytes: 12, Objects: 1, ExpiresAt: now.Add(time.Hour), ObservedAt: now,
		}
	}
	for index, reference := range references {
		organization := organizations[index]
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			created, err := w.AppendArtifactReference(t.Context(), reference)
			if err != nil {
				return err
			}
			created.ExpiresAt = created.ExpiresAt.UTC()
			created.ObservedAt = created.ObservedAt.UTC()
			if !reflect.DeepEqual(created, reference) {
				t.Fatalf("created reference = %+v, want %+v", created, reference)
			}
			_, err = w.AppendArtifactReference(t.Context(), reference)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, reference := range references {
		organization := organizations[index]
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.ArtifactReference(t.Context(), tracker.ProjectID(reference.ProjectID), tracker.NativeWorkItemID(reference.WorkItemID), reference.ArtifactID, reference.Revision)
			if err != nil {
				return err
			}
			got.ExpiresAt = got.ExpiresAt.UTC()
			got.ObservedAt = got.ObservedAt.UTC()
			if !reflect.DeepEqual(got, reference) {
				t.Fatalf("stored reference = %+v, want %+v", got, reference)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	changed := references[0]
	changed.SHA256 = artifact.Digest([]byte("changed"))
	err := repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendArtifactReference(t.Context(), changed)
		return err
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("changed immutable reference error = %v, want revision conflict", err)
	}
	if err := repository.ReadTenant(t.Context(), organizations[1], corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.ArtifactReference(t.Context(), "prj_first", "wi_first_1", references[0].ArtifactID, 1)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("cross-tenant reference lookup = %+v, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewedFilesUseTypedTenantRecords(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, organization := range organizations {
		seedRepository(t, repository, organization)
	}
	for index, organization := range organizations {
		suffix := fmt.Sprintf("viewed_%d", index+1)
		fixture := seedHistory(t, repository, organization, suffix)
		actor := "reviewer_" + suffix
		record := eventstore.ViewedFile{
			OrganizationID: organization, ProjectID: fixture.change.ProjectID, ChangeID: fixture.change.ID,
			VersionID: fixture.version.ID, PrincipalID: actor, ManifestSHA256: artifact.Digest([]byte("manifest_" + suffix)),
			FileSHA256: artifact.Digest([]byte("file_" + suffix)), Viewed: true, UpdatedAt: historyAt(),
		}
		if err := repository.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
			stored, err := w.SetViewedFile(t.Context(), record)
			stored.UpdatedAt = stored.UpdatedAt.UTC()
			if err == nil && !reflect.DeepEqual(stored, record) {
				t.Errorf("stored viewed file = %+v, want %+v", stored, record)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
			rows, err := r.ViewedFiles(t.Context(), fixture.version.ID, actor, 2)
			if err != nil {
				return err
			}
			if len(rows) == 1 {
				rows[0].UpdatedAt = rows[0].UpdatedAt.UTC()
			}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], record) {
				t.Errorf("viewed files = %+v, want %+v", rows, record)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
