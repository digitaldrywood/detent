package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCutoverContractAtomicallyPinsSourcesAndDestinationMappings(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	objects := newHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := seedCoreOrganization(t.Context(), w, org); err != nil {
				return err
			}
			for _, id := range []string{"first", "second"} {
				project := tracker.ProjectID("prj_" + id)
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: id, Profile: "github_compatible", ContentSettingsRevision: 2}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
					return err
				}
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID("wi_" + id), Number: 1, Title: "Example"}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		archive, err := objects.StoreDetail(t.Context(), string(org), []byte("example import receipt"))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			project := tracker.ProjectID("prj_" + id)
			checksum := artifact.Digest([]byte(id))
			commit := postgres.CutoverCommit{ExpectedContentRevision: 2, Receipt: pgquery.StoreCutoverReceiptParams{OrganizationID: org, ProjectID: project, Checkpoint: checksum, SourceProfile: "github_compatible", ExpectedRevision: 1, ReceiptObjectKey: archive.Key, ReceiptSha256: archive.SHA256, ReceiptBytes: archive.CompressedSize, ReceiptContentBytes: archive.UncompressedSize, ActorID: "example", CreatedAt: now}}
			stale := commit
			stale.Receipt.ExpectedRevision = 4
			if err := repository.WriteCutover(t.Context(), org, corestore.Serving, func(w postgres.CutoverWriter) error { _, err := w.CommitProjectCutover(t.Context(), stale); return err }); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale cutover=%v", err)
			}
			if err := repository.ReadCutover(t.Context(), org, corestore.Serving, func(r postgres.CutoverReader) error {
				row, err := r.ProjectRecord(t.Context(), project)
				if err == nil && row.Core.Profile != "github_compatible" {
					t.Fatal("failed receipt changed project profile")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := repository.WriteCutover(t.Context(), org, corestore.Serving, func(w postgres.CutoverWriter) error {
					if _, err := w.CommitProjectCutover(t.Context(), commit); err != nil {
						return err
					}
					if _, err := w.BindNeutralImportSource(t.Context(), postgres.BindNeutralImportSourceInput{OrganizationID: org, SourceID: id, SchemaVersion: 1, Checksum: checksum, SchemaDigest: checksum, VerifiedAt: now}); err != nil {
						return err
					}
					if _, err := w.PutNeutralImportRecord(t.Context(), postgres.PutNeutralImportRecordInput{OrganizationID: org, SourceID: id, Family: "work_item", SourceKey: id, RecordDigest: checksum, DestinationID: "wi_" + id, SourceRevision: 7, PayloadObjectKey: archive.Key, PayloadSHA256: archive.SHA256, PayloadBytes: archive.CompressedSize, PayloadContentBytes: archive.UncompressedSize, ImportedAt: now}); err != nil {
						return err
					}
					_, err := w.PutLocalImportMapping(t.Context(), postgres.PutLocalImportMappingInput{OrganizationID: org, ProjectID: project, SourceKey: id, SourceKind: "neutral", SourceDigest: checksum, WorkItemID: "wi_" + id, ImportedAt: now})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, test := range []struct {
		name  string
		input postgres.BindNeutralImportSourceInput
		want  error
	}{
		{"changed source", postgres.BindNeutralImportSourceInput{OrganizationID: "org_first", SourceID: "first", SchemaVersion: 1, Checksum: artifact.Digest([]byte("changed")), SchemaDigest: artifact.Digest([]byte("first")), VerifiedAt: now}, corestore.ErrRevisionConflict},
		{"foreign source", postgres.BindNeutralImportSourceInput{OrganizationID: "org_second"}, corestore.ErrTenantMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := repository.WriteCutover(t.Context(), "org_first", corestore.Serving, func(w postgres.CutoverWriter) error {
				_, err := w.BindNeutralImportSource(t.Context(), test.input)
				return err
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("source pin=%v", err)
			}
		})
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadCutover(t.Context(), org, corestore.Serving, func(r postgres.CutoverReader) error {
			for _, id := range []string{"first", "second"} {
				project := tracker.ProjectID("prj_" + id)
				record, err := r.ProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				if record.Core.Profile != "native" || record.Settings.Intake != "manual" || record.Settings.Revision != 2 {
					t.Fatalf("cutover project=%+v", record)
				}
				receipt, err := r.CutoverReceipt(t.Context(), postgres.CutoverReceiptInput{OrganizationID: org, ProjectID: project, Checkpoint: artifact.Digest([]byte(id))})
				if err != nil {
					return err
				}
				if receipt.OrganizationID != org || receipt.SourceProfile != "github_compatible" {
					t.Fatalf("receipt=%+v", receipt)
				}
				mapping, err := r.LocalImportMapping(t.Context(), postgres.LocalImportMappingInput{OrganizationID: org, ProjectID: project, SourceKey: id})
				if err != nil {
					return err
				}
				if mapping.OrganizationID != org || mapping.WorkItemID != "wi_"+id {
					t.Fatalf("mapping=%+v", mapping)
				}
				imported, err := r.NeutralImportRecord(t.Context(), postgres.NeutralImportRecordInput{OrganizationID: org, SourceID: id, Family: "work_item", SourceKey: id})
				if err != nil {
					return err
				}
				if imported.SourceRevision != 7 || imported.DestinationID != "wi_"+id {
					t.Fatalf("neutral record=%+v", imported)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
