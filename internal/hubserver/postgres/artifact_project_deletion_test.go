package postgres_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactProjectDeletionRetainsCatalogCustody(t *testing.T) {
	for _, state := range []string{"complete", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			admin := template.Database(t)
			repository, _ := repositoryFor(t, admin, smallPool())
			now := time.Now().UTC().Truncate(time.Microsecond)
			organizations := []tracker.OrganizationID{tracker.OrganizationID(artifact.NewID("org")), tracker.OrganizationID(artifact.NewID("org"))}
			projects := []tracker.ProjectID{tracker.ProjectID(artifact.NewID("prj")), tracker.ProjectID(artifact.NewID("prj"))}
			type deletionCase struct {
				scope     postgres.CatalogScope
				reference artifact.Reference
			}
			var fixtures []deletionCase
			for _, org := range organizations {
				publisherID := artifact.NewID("publisher")
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := seedCoreOrganization(t.Context(), w, org); err != nil {
						return err
					}
					return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: publisherID, Name: "Example publisher", Hash: artifact.Digest([]byte(publisherID)), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
				for _, project := range projects {
					service := artifact.NewID("service")
					item := tracker.NativeWorkItemID(artifact.NewID("wi"))
					runID, attemptID := artifact.NewID("run"), artifact.NewID("attempt")
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
							return err
						}
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: 1, Title: "Example", WorkflowStateName: new("Todo")}); err != nil {
							return err
						}
						if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, ProjectID: project, TokenID: publisherID}); err != nil {
							return err
						}
						return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: service, Origin: "https://example.invalid", Mode: "customer", PublisherTokenID: publisherID})
					}); err != nil {
						t.Fatal(err)
					}
					scope := postgres.CatalogScope{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: artifact.NewID("artifact")}
					manifest := postgres.CatalogManifest{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: scope.UploadID, Revision: 1, ID: artifact.NewID("manifest"), State: state, Sha256: artifact.Digest([]byte("manifest")), BodyObjectKey: "objects/" + string(org) + "/" + string(project), BodyBytes: 1, RetentionPolicyID: "example", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), ObservedAt: now}
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						reservation := catalogReservation(org, project, service, now)
						reservation.Upload.ID, reservation.Upload.WorkItemID = scope.UploadID, string(item)
						reservation.Upload.RunID, reservation.Upload.AttemptID = runID, attemptID
						if _, err := w.ReserveCatalogUpload(t.Context(), reservation); err != nil {
							return err
						}
						if err := prepareCatalogManifest(t, w, &manifest); err != nil {
							return err
						}
						_, err := w.CommitCatalogManifest(t.Context(), manifest)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					reference := artifact.Reference{SchemaVersion: 1, Scope: artifact.Scope{OrganizationID: string(org), ProjectID: string(project), WorkItemID: string(item), RunID: runID, AttemptID: attemptID}, ServiceID: service, ArtifactID: scope.UploadID, ManifestID: manifest.ID, Revision: 1, SHA256: manifest.Sha256, Kind: "log", State: state, Availability: "available", ExpiresAt: manifest.ExpiresAt, ObservedAt: now}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						var err error
						reference, err = w.AppendArtifactReference(t.Context(), reference)
						if err != nil {
							return err
						}
						return w.CreateArtifactReadGrant(t.Context(), artifactstore.ReadGrant{ProjectID: project, WorkItemID: item, TokenHash: artifact.Digest([]byte(string(org) + "/" + string(project))), PrincipalID: publisherID, ServiceID: service, ArtifactID: scope.UploadID, Revision: 1, ExpiresAt: now.Add(4 * time.Hour)}, now)
					}); err != nil {
						t.Fatal(err)
					}
					fixtures = append(fixtures, deletionCase{scope, reference})
				}
			}
			deleted := map[string]bool{}
			for _, fixture := range fixtures {
				scope := fixture.scope
				org, project, service := scope.OrganizationID, scope.ProjectID, scope.ServiceID
				assertHeld := func(stage string) {
					t.Helper()
					for _, check := range []struct {
						name string
						run  func(tenantstore.Writer) error
					}{
						{"idle", func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), project, now) }},
						{"retire", func(w tenantstore.Writer) error { return w.RetireProject(t.Context(), project, 1, now) }},
						{"delete", func(w tenantstore.Writer) error { return w.DeleteProject(t.Context(), project) }},
					} {
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, check.run); !errors.Is(err, projectstore.ErrActiveWork) {
							t.Fatalf("%s %s tenant=%s project=%s: %v", stage, check.name, org, project, err)
						}
					}
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.ArtifactService(t.Context(), project, service)
						return err
					}); err != nil {
						t.Fatalf("%s lost binding: %v", stage, err)
					}
				}
				assertHeld(state)
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
					if _, err := w.BeginCatalogDeletion(t.Context(), scope); err != nil {
						return err
					}
					_, err := w.FinishCatalogDeletion(t.Context(), postgres.CatalogDeletion{CatalogScope: scope, At: now.Add(time.Hour), ReceiptObjectKey: "objects/" + string(project) + "/deletion"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				assertHeld("deleted tombstone")
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
					rows, err := w.CatalogManifestIntents(t.Context(), postgres.CatalogManifestIntentsInput{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: scope.UploadID, RetiredOnly: true, Limit: 32})
					if err != nil {
						return err
					}
					for _, row := range rows {
						if err := w.PruneCatalogManifestIntent(t.Context(), postgres.CatalogIntentTransition{CatalogScope: scope, ID: row.ID, ReceiptObjectKey: row.ReceiptObjectKey}); err != nil {
							return err
						}
					}
					return w.PruneCatalogUpload(t.Context(), scope, now.Add(time.Hour))
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					if _, err := w.PutCatalogTrafficSettings(t.Context(), postgres.CatalogTrafficPolicy{OrganizationID: org, ProjectID: project, ServiceID: service, WindowSeconds: 60, RetentionSeconds: 3600}); err != nil {
						return err
					}
					return w.RecordCatalogTraffic(t.Context(), postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: service, ID: string(project), Minute: now.Truncate(time.Minute), Requests: 1})
				}); err != nil {
					t.Fatal(err)
				}
				assertHeld("retained traffic")
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
					return w.PruneCatalogTraffic(t.Context(), now.Add(time.Minute), 1)
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.RequireProjectIdle(t.Context(), project, now); err != nil {
						return err
					}
					if err := w.(accessstore.ProjectAccessCleaner).DeleteAccessProjectGrants(t.Context(), project); err != nil {
						return err
					}
					return w.RetireProject(t.Context(), project, 1, now.Add(2*time.Hour))
				}); err != nil {
					t.Fatalf("accounted catalog prevented deletion tenant=%s project=%s: %v", org, project, err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.ArtifactService(t.Context(), project, service)
					return err
				}); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("deleted project retained binding: %v", err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					record, err := r.Project(t.Context(), project)
					if err != nil {
						return err
					}
					if record.DeletedAt == nil {
						t.Error("accounted project was not retired")
					}
					reference, err := r.ArtifactReference(t.Context(), project, tracker.NativeWorkItemID(fixture.reference.WorkItemID), scope.UploadID, 1)
					if err == nil && !reflect.DeepEqual(reference, fixture.reference) {
						t.Errorf("retirement changed immutable reference: got=%+v want=%+v", reference, fixture.reference)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.ArtifactReadGrant(t.Context(), project, artifact.Digest([]byte(string(org)+"/"+string(project))))
					return err
				}); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("retired project retained read grant: %v", err)
				}
				deleted[string(org)+"/"+string(project)] = true
				for _, other := range fixtures {
					if deleted[string(other.scope.OrganizationID)+"/"+string(other.scope.ProjectID)] {
						continue
					}
					if err := repository.ReadTenant(t.Context(), other.scope.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.ArtifactService(t.Context(), other.scope.ProjectID, other.scope.ServiceID)
						return err
					}); err != nil {
						t.Fatalf("deletion crossed tenant/project binding: %v", err)
					}
					if err := repository.ReadArtifactCatalog(t.Context(), other.scope.OrganizationID, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
						_, err := r.CatalogUpload(t.Context(), postgres.CatalogUploadInput{OrganizationID: other.scope.OrganizationID, ProjectID: other.scope.ProjectID, ServiceID: other.scope.ServiceID, ID: other.scope.UploadID})
						return err
					}); err != nil {
						t.Fatalf("deletion crossed tenant/project catalog: %v", err)
					}
				}
			}

		})
	}
}

func TestArtifactProjectGrantIndexResumesAfterInterruption(t *testing.T) {
	assertConcurrentIndexMigrationRecovery(t, 20261109035756, "artifact_grants_project", "CREATE INDEX artifact_grants_project ON artifact_grants (token_hash)", "CREATE INDEX artifact_grants_project ON public.artifact_grants USING btree (organization_id, project_id, token_hash)")
}
