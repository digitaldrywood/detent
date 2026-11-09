package postgres_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactCatalogManifestIntentsFenceDiscardAndQuota(t *testing.T) {
	_, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
				service := fmt.Sprintf("service_%032x", serviceIndex)
				reservation := catalogReservation(org, project, service, now)
				scope := postgres.CatalogScope{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload"}
				intent := postgres.CatalogManifestIntent{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", ID: "candidate", Revision: 1, BodyObjectKey: "candidate/" + string(org) + "/" + string(project) + "/" + service, Sha256: artifact.Digest([]byte("manifest")), BodyBytes: 800, ManifestState: "complete"}
				write := func(f func(postgres.ArtifactCatalogWriter) error) error {
					return repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, f)
				}
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.ReserveCatalogUpload(t.Context(), reservation)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					row, err := w.PrepareCatalogManifestIntent(t.Context(), intent, now)
					if err == nil {
						intent = row
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					replay := intent
					replay.ID = "proposed-again"
					replay.BodyObjectKey += "/again"
					row, err := w.PrepareCatalogManifestIntent(t.Context(), replay, now)
					if err == nil && (row.ID != intent.ID || row.BodyObjectKey != intent.BodyObjectKey) {
						t.Fatal("intent replay allocated another external body")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				object := postgres.CatalogObject{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", ID: "object", Sequence: 0, Size: 201, MediaType: "text/plain", Sha256: artifact.Digest([]byte("object"))}
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.PutCatalogObject(t.Context(), object, now)
					return err
				}); !errors.Is(err, artifact.ErrQuota) {
					t.Fatal("manifest intent did not reserve its bytes", err)
				}
				object.Size = 200
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.PutCatalogObject(t.Context(), object, now)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				transition := postgres.CatalogIntentTransition{CatalogScope: scope, ID: intent.ID, State: "discarding"}
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.TransitionCatalogManifestIntent(t.Context(), transition)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				transition.State, transition.StorageVersion = "verified", "example-version"
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.TransitionCatalogManifestIntent(t.Context(), transition)
					return err
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatal("discard fence allowed manifest verification", err)
				}
				transition.State = "discarded"
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.TransitionCatalogManifestIntent(t.Context(), transition)
					return err
				}); !errors.Is(err, artifact.ErrInvalid) {
					t.Fatal("discard released bytes without receipt", err)
				}
				transition.ReceiptObjectKey = "receipt/" + intent.BodyObjectKey
				for range 2 {
					if err := write(func(w postgres.ArtifactCatalogWriter) error {
						_, err := w.TransitionCatalogManifestIntent(t.Context(), transition)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				intent.ID = "replacement"
				intent.BodyObjectKey += "/replacement"
				if err := write(func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.PrepareCatalogManifestIntent(t.Context(), intent, now)
					return err
				}); err != nil {
					t.Fatal("discard receipt did not release exactly one allocation", err)
				}
			}
		}
	}
}

func TestArtifactCatalogIntentHistoryReleasesSpaceInBoundedPages(t *testing.T) {
	_, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			service := fmt.Sprintf("service_%032x", 1)
			scope := postgres.CatalogScope{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload"}
			var first postgres.CatalogManifestIntent
			if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
				if _, err := w.ReserveCatalogUpload(t.Context(), catalogReservation(org, project, service, now)); err != nil {
					return err
				}
				for range artifact.MaxIntentHistory {
					candidate := postgres.CatalogManifestIntent{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", ID: "proposed", Revision: 1, BodyObjectKey: "proposed", Sha256: artifact.Digest([]byte("manifest")), BodyBytes: 100, ManifestState: "complete"}
					row, err := w.PrepareCatalogManifestIntent(t.Context(), candidate, now)
					if err != nil {
						return err
					}
					if first.ID == "" {
						first = row
					}
					transition := postgres.CatalogIntentTransition{CatalogScope: scope, ID: row.ID, State: "discarding"}
					if _, err := w.TransitionCatalogManifestIntent(t.Context(), transition); err != nil {
						return err
					}
					transition.State, transition.ReceiptObjectKey = "discarded", row.BodyObjectKey+"/receipt"
					if _, err := w.TransitionCatalogManifestIntent(t.Context(), transition); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
				_, err := w.PrepareCatalogManifestIntent(t.Context(), first, now)
				return err
			}); !errors.Is(err, artifact.ErrQuota) {
				t.Fatal("discarded history grew beyond its admission bound", err)
			}
			for page := range 2 {
				var rows []postgres.CatalogManifestIntent
				if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
					var err error
					rows, err = r.CatalogManifestIntents(t.Context(), postgres.CatalogManifestIntentsInput{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", Limit: 32, RetiredOnly: true})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 32 {
					t.Fatalf("history page %d contains %d rows, want 32", page, len(rows))
				}
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					for _, row := range rows {
						if err := w.PruneCatalogManifestIntent(t.Context(), postgres.CatalogIntentTransition{CatalogScope: scope, ID: row.ID, ReceiptObjectKey: row.ReceiptObjectKey}); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
				row, err := w.PrepareCatalogManifestIntent(t.Context(), first, now)
				if err == nil && (row.ID == first.ID || row.BodyObjectKey == first.BodyObjectKey) {
					t.Fatal("pruning allowed a stale request to recreate the old physical write identity")
				}
				return err
			}); err != nil {
				t.Fatal("bounded cleanup did not restore candidate admission", err)
			}
		}
	}
}
