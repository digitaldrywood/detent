package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactCatalogScopedPagesKeepRejectedPublicationProgress(t *testing.T) {
	_, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
				service := fmt.Sprintf("service_%032x", serviceIndex)
				for index := range 33 {
					request := catalogReservation(org, project, service, now)
					request.Upload.ID = fmt.Sprintf("upload_%02d", index)
					request.Upload.IdempotencyKey = request.Upload.ID
					manifest := postgres.CatalogManifest{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: request.Upload.ID, Revision: 1, ID: fmt.Sprintf("manifest_%02d", index), State: "complete", Sha256: artifact.Digest([]byte("manifest")), BodyObjectKey: fmt.Sprintf("%s/%s/%s/%d", org, project, service, index), BodyBytes: 100, RetentionPolicyID: "example", CreatedAt: now, ExpiresAt: request.Upload.ExpiresAt, ObservedAt: now}
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						if _, err := w.ReserveCatalogUpload(t.Context(), request); err != nil {
							return err
						}
						if err := prepareCatalogManifest(t, w, manifest); err != nil {
							return err
						}
						_, err := w.CommitCatalogManifest(t.Context(), manifest)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
			service := fmt.Sprintf("service_%032x", serviceIndex)
			cursorProject, cursorID := "", ""
			seen := map[string]bool{}
			for page := 0; page < 3; page++ {
				var rows []postgres.CatalogManifest
				if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
					var err error
					rows, err = r.ScopedPendingCatalogManifests(t.Context(), postgres.ScopedPendingCatalogManifestsInput{OrganizationID: org, ServiceID: service, PublisherID: "first", Now: now, AfterProject: cursorProject, AfterID: cursorID, PageLimit: 32})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				expected := 32
				if page == 2 {
					expected = 2
				}
				if len(rows) != expected {
					t.Fatalf("publication page %d has %d rows, want %d", page, len(rows), expected)
				}
				for _, row := range rows {
					identity := string(row.ProjectID) + "/" + row.ID
					if row.OrganizationID != org || row.ServiceID != service || seen[identity] {
						t.Fatal("publication cursor crossed scope or repeated a rejected receipt")
					}
					seen[identity] = true
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						_, err := w.RecordCatalogPublication(t.Context(), postgres.CatalogPublicationResult{OrganizationID: org, ProjectID: row.ProjectID, ServiceID: service, ManifestID: row.ID, Outcome: "uncertain", UpdatedAt: now.Add(time.Minute)})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					cursorProject, cursorID = string(row.ProjectID), row.ID
				}
			}
			if len(seen) != 66 {
				t.Fatal("later project's publications starved behind rejected receipts")
			}
		}
	}
}

func TestArtifactCatalogRetentionUsesEachProjectPolicy(t *testing.T) {
	_, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
				service := fmt.Sprintf("service_%032x", serviceIndex)
				request := catalogReservation(org, project, service, now)
				retention := int64(120)
				if project == "prj_second" {
					retention = 240
				}
				policy := artifact.Policy{ID: "example", Limits: artifact.Limits{RetainedBytes: 100000, ReservedBytes: 100000, ArtifactBytes: 1000, UploadBytes: 1000, RetentionSeconds: 3600}, AbandonedUploadSeconds: 30, BackupSeconds: 60, DeletionRecordSeconds: retention}
				scope := postgres.CatalogScope{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: request.Upload.ID}
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					if _, err := w.SetCatalogPolicy(t.Context(), project, service, policy); err != nil {
						return err
					}
					if _, err := w.ReserveCatalogUpload(t.Context(), request); err != nil {
						return err
					}
					if _, err := w.BeginCatalogDeletion(t.Context(), scope); err != nil {
						return err
					}
					_, err := w.FinishCatalogDeletion(t.Context(), postgres.CatalogDeletion{CatalogScope: scope, At: now, ReceiptObjectKey: "example/receipt"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
			service := fmt.Sprintf("service_%032x", serviceIndex)
			for _, boundary := range []struct {
				seconds int
				want    int
			}{{119, 0}, {120, 1}, {240, 2}} {
				at := now.Add(time.Duration(boundary.seconds) * time.Second)
				if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
					rows, err := r.ScopedCatalogRetention(t.Context(), postgres.ScopedCatalogRetentionInput{OrganizationID: org, ServiceID: service, PublisherID: "first", Category: 3, Now: at, Before: at, PageLimit: 32})
					if err != nil {
						return err
					}
					if len(rows) != boundary.want {
						t.Fatalf("retention boundary %d: got %d tombstones, want %d", boundary.seconds, len(rows), boundary.want)
					}
					if len(rows) > 0 && rows[0].ProjectID != "prj_first" {
						t.Fatal("retention used another project's policy or changed seek order")
					}
					if len(rows) > 1 && rows[1].ProjectID != "prj_second" {
						t.Fatal("retention crossed project or repeated a tombstone")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
