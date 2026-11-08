package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func artifactCatalogFixture(t *testing.T) (*sql.DB, *postgres.Repository, *pgxpool.Pool, time.Time) {
	t.Helper()
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	now := historyAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			seedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []string{"first", "second"} {
				project := tracker.ProjectID("prj_" + id)
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "first", ProjectID: project}); err != nil {
					return err
				}
				for i := 1; i <= 2; i++ {
					if err := w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: fmt.Sprintf("service_%032x", i), Origin: "https://example.invalid", Mode: "customer", PublisherTokenID: "first"}); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return admin, repository, pool, now
}
func catalogReservation(org tracker.OrganizationID, project tracker.ProjectID, service string, now time.Time) postgres.CatalogReservation {
	item := "wi_first_1"
	if project == "prj_second" {
		item = "wi_second_1"
	}
	return postgres.CatalogReservation{Upload: postgres.CatalogUpload{OrganizationID: org, ProjectID: project, ServiceID: service, ID: "upload", IdempotencyKey: "request", RequestHash: artifact.Digest([]byte("request")), WorkItemID: item, Kind: "log", RequestedBytes: 1000, PartLimit: 1000, CreatedAt: now, UploadDeadline: now.Add(time.Hour), ExpiresAt: now.Add(24 * time.Hour)}, ReservedLimit: 100000, RetainedLimit: 100000}
}
func TestArtifactCatalogContractAccountsReceiptsOnceAcrossReopen(t *testing.T) {
	admin, repository, pool, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, suffix := range []string{"first", "second"} {
			for i := 1; i <= 2; i++ {
				project := tracker.ProjectID("prj_" + suffix)
				service := fmt.Sprintf("service_%032x", i)
				reservation := catalogReservation(org, project, service, now)
				for range 2 {
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						row, err := w.ReserveCatalogUpload(t.Context(), reservation)
						if err == nil && row.ReservedBytes != 1000 {
							t.Fatalf("reservation=%+v", row)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), project, now) }); !errors.Is(err, projectstore.ErrActiveWork) {
					t.Fatalf("active catalog ignored by deletion: %v", err)
				}
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					for sequence, size := range []int64{20, 30} {
						object := postgres.CatalogObject{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", Sequence: int32(sequence), ID: fmt.Sprintf("object_%d", sequence), MediaType: "text/plain", Size: size, Sha256: artifact.Digest([]byte(fmt.Sprint(sequence))), ByteOffset: int64(sequence) * 20}
						if _, err := w.PutCatalogObject(t.Context(), object, now); err != nil {
							return err
						}
						object.StorageVersion = "version"
						for range 2 {
							verified, err := w.VerifyCatalogObject(t.Context(), object, now)
							if err != nil {
								return err
							}
							if verified.VerifiedAt == nil {
								t.Fatal("object not verified")
							}
						}
					}
					manifest := postgres.CatalogManifest{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", Revision: 1, ID: "manifest", State: "complete", Sha256: artifact.Digest([]byte("manifest")), BodyObjectKey: "objects/manifest/" + string(org) + "/" + suffix + "/" + service, BodyBytes: 100, TotalBytes: 50, Objects: 2, RetentionPolicyID: "example", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), ObservedAt: now}
					for range 2 {
						if _, err := w.CommitCatalogManifest(t.Context(), manifest); err != nil {
							return err
						}
					}
					result := postgres.CatalogPublicationResult{OrganizationID: org, ProjectID: project, ServiceID: service, ManifestID: "manifest", Outcome: "committed", ExternalReceipt: "receipt", UpdatedAt: now}
					if suffix == "first" && i == 1 {
						result.Outcome = "uncertain"
						result.ExternalReceipt = ""
					}
					if _, err := w.RecordCatalogPublication(t.Context(), result); err != nil {
						return err
					}
					if result.Outcome == "committed" {
						if _, err := w.RecordCatalogPublication(t.Context(), result); err != nil {
							return err
						}
					}
					if _, err := w.PutCatalogTrafficSettings(t.Context(), postgres.CatalogTrafficPolicy{OrganizationID: org, ProjectID: project, ServiceID: service, WindowSeconds: 3600, RetentionSeconds: 86400}); err != nil {
						return err
					}
					traffic := postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: service, ID: "traffic", Minute: now.UTC().Truncate(time.Minute), UploadBytes: 10, DownloadBytes: 20, Requests: 2}
					for range 2 {
						if err := w.RecordCatalogTraffic(t.Context(), traffic); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*postgres.CatalogReservation)
		want   error
	}{
		{"foreign tenant", func(r *postgres.CatalogReservation) { r.Upload.OrganizationID = "org_second" }, corestore.ErrTenantMismatch},
		{"changed replay", func(r *postgres.CatalogReservation) { r.Upload.RequestHash = artifact.Digest([]byte("changed")) }, corestore.ErrRevisionConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := catalogReservation("org_first", "prj_first", fmt.Sprintf("service_%032x", 1), now)
			test.mutate(&request)
			err := repository.WriteArtifactCatalog(t.Context(), "org_first", corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
				_, err := w.ReserveCatalogUpload(t.Context(), request)
				return err
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("reservation error=%v want=%v", err, test.want)
			}
		})
	}
	var count int
	if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM artifact_catalog_uploads`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound catalog count=%d error=%v", count, err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
			usage, err := r.CatalogUsage(t.Context())
			if err != nil {
				return err
			}
			if usage.ReservedBytes != 0 || usage.RetainedBytes != 600 {
				t.Fatalf("duplicate accounting=%+v", usage)
			}
			for _, check := range []struct {
				at   time.Time
				want int
			}{{now.Add(time.Minute), 0}, {now.Add(24 * time.Hour), 4}} {
				due, err := r.CatalogRetentionCandidates(t.Context(), postgres.CatalogRetentionCandidatesInput{OrganizationID: org, Now: check.at, PageLimit: 20})
				if err != nil {
					return err
				}
				if len(due) != check.want {
					t.Fatalf("retention at=%v rows=%d want=%d", check.at, len(due), check.want)
				}
			}

			pending, err := r.PendingCatalogPublications(t.Context(), postgres.PendingCatalogPublicationsInput{OrganizationID: org, Limit: 20})
			if err != nil {
				return err
			}
			if len(pending) != 1 || pending[0].Outcome != "uncertain" {
				t.Fatalf("pending publication=%+v", pending)
			}
			for _, suffix := range []string{"first", "second"} {
				for i := 1; i <= 2; i++ {
					project := tracker.ProjectID("prj_" + suffix)
					service := fmt.Sprintf("service_%032x", i)
					upload, err := r.CatalogUpload(t.Context(), postgres.CatalogUploadInput{OrganizationID: org, ProjectID: project, ServiceID: service, ID: "upload"})
					if err != nil {
						return err
					}
					if upload.OrganizationID != org || upload.State != "complete" || upload.RetainedBytes != 150 || upload.ManifestRevision != 1 {
						t.Fatalf("upload=%+v", upload)
					}
					objects, err := r.CatalogObjects(t.Context(), postgres.CatalogObjectsInput{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", Limit: 10})
					if err != nil {
						return err
					}
					if len(objects) != 2 || objects[0].VerifiedAt == nil || objects[1].VerifiedAt == nil {
						t.Fatalf("objects=%+v", objects)
					}
					manifest, err := r.CatalogManifest(t.Context(), postgres.CatalogManifestInput{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", Revision: 1})
					if err != nil {
						return err
					}
					if manifest.OrganizationID != org || manifest.TotalBytes != 50 || manifest.Objects != 2 {
						t.Fatalf("manifest=%+v", manifest)
					}
					policy, err := r.CatalogTrafficSettings(t.Context(), postgres.CatalogTrafficSettingsInput{OrganizationID: org, ProjectID: project, ServiceID: service})
					if err != nil {
						return err
					}
					if policy.WindowSeconds != 3600 || policy.RetentionSeconds != 86400 {
						t.Fatalf("traffic policy=%+v", policy)
					}

					traffic, err := r.CatalogTraffic(t.Context(), postgres.CatalogTrafficInput{OrganizationID: org, ProjectID: project, ServiceID: service, Minute: now.UTC().Truncate(time.Minute), Minute_2: now.Add(time.Hour), Limit: 10})
					if err != nil {
						return err
					}
					if len(traffic) != 1 || traffic[0].UploadBytes != 10 || traffic[0].DownloadBytes != 20 || traffic[0].Requests != 2 {
						t.Fatalf("traffic replay=%+v", traffic)
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		scope := postgres.CatalogScope{OrganizationID: org, ProjectID: "prj_first", ServiceID: fmt.Sprintf("service_%032x", 1), UploadID: "upload"}
		for range 2 {
			if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
				if _, err := w.BeginCatalogDeletion(t.Context(), scope); err != nil {
					return err
				}
				_, err := w.FinishCatalogDeletion(t.Context(), postgres.CatalogDeletion{CatalogScope: scope, At: now.Add(time.Hour), ReceiptObjectKey: "objects/deletion"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
			usage, err := r.CatalogUsage(t.Context())
			if err == nil && (usage.ReservedBytes != 0 || usage.RetainedBytes != 450) {
				t.Fatalf("deletion double released=%+v", usage)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error { return w.PruneCatalogUpload(t.Context(), scope, now) }); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("early tombstone prune=%v", err)
		}
		if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
			return w.PruneCatalogUpload(t.Context(), scope, now.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
			_, err := r.CatalogManifest(t.Context(), postgres.CatalogManifestInput{OrganizationID: org, ProjectID: scope.ProjectID, ServiceID: scope.ServiceID, UploadID: scope.UploadID, Revision: 1})
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("pruned manifest remains=%v", err)
		}

	}
}
func TestArtifactCatalogContractSerializesQuotaAndRequiresDiscardReceipt(t *testing.T) {
	_, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for i := 1; i <= 2; i++ {
			workers.Go(func() {
				<-start
				request := catalogReservation(org, "prj_first", fmt.Sprintf("service_%032x", i), now)
				request.ReservedLimit = 1000
				request.RetainedLimit = 1000
				results <- repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					_, err := w.ReserveCatalogUpload(t.Context(), request)
					return err
				})
			})
		}
		close(start)
		workers.Wait()
		close(results)
		success, quota := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, artifact.ErrQuota) {
				quota++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || quota != 1 {
			t.Fatalf("quota outcomes success=%d rejected=%d", success, quota)
		}
		var winner postgres.CatalogUpload
		if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
			for i := 1; i <= 2; i++ {
				row, err := r.CatalogUpload(t.Context(), postgres.CatalogUploadInput{OrganizationID: org, ProjectID: "prj_first", ServiceID: fmt.Sprintf("service_%032x", i), ID: "upload"})
				if err == nil {
					winner = row
					return nil
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			return errors.New("reserved upload missing")
		}); err != nil {
			t.Fatal(err)
		}
		object := postgres.CatalogObject{OrganizationID: org, ProjectID: "prj_first", ServiceID: winner.ServiceID, UploadID: "upload", ID: "pending", Sequence: 0, MediaType: "text/plain", Size: 10, Sha256: artifact.Digest([]byte("pending"))}
		manifest := postgres.CatalogManifest{OrganizationID: org, ProjectID: "prj_first", ServiceID: winner.ServiceID, UploadID: "upload", Revision: 1, ID: "interrupted", State: "interrupted", Sha256: artifact.Digest([]byte("interrupted")), BodyObjectKey: "objects/interrupted", BodyBytes: 100, RetentionPolicyID: "example", CreatedAt: now, ExpiresAt: winner.ExpiresAt, ObservedAt: now.Add(2 * time.Hour)}
		if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
			_, err := w.PutCatalogObject(t.Context(), object, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
			_, err := w.CommitCatalogManifest(t.Context(), manifest)
			return err
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("uncertain object released quota=%v", err)
		}
		if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Background, func(w postgres.ArtifactCatalogWriter) error {
			if _, err := w.DiscardCatalogObject(t.Context(), object, now.Add(time.Hour), "objects/definite-rejection"); err != nil {
				return err
			}
			if _, err := w.CommitCatalogManifest(t.Context(), manifest); err != nil {
				return err
			}
			usage, err := w.CatalogUsage(t.Context())
			if err == nil && (usage.ReservedBytes != 0 || usage.RetainedBytes != 100) {
				t.Fatalf("interrupted accounting=%+v", usage)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArtifactCatalogPendingAdmission(t *testing.T) {
	for _, scenario := range []string{"outstanding descriptors", "stored part limit", "concurrent descriptors"} {
		t.Run(scenario, func(t *testing.T) {
			admin, repository, _, now := artifactCatalogFixture(t)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
					service := fmt.Sprintf("service_%032x", 1)
					request := catalogReservation(org, project, service, now)
					if scenario == "stored part limit" {
						request.Upload.PartLimit = 100
					}
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						_, err := w.ReserveCatalogUpload(t.Context(), request)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					object := postgres.CatalogObject{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: "upload", Sequence: 0, ID: "pending_0", MediaType: "text/plain", Size: 800, Sha256: artifact.Digest([]byte("pending_0"))}
					put := func(value postgres.CatalogObject) error {
						return repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
							_, err := w.PutCatalogObject(t.Context(), value, now)
							return err
						})
					}
					if scenario == "stored part limit" {
						object.Size = 101
						if err := put(object); !errors.Is(err, artifact.ErrQuota) {
							t.Fatalf("descriptor above stored part limit admitted: %v", err)
						}
						object.Size = 100
						if err := put(object); err != nil {
							t.Fatalf("descriptor at stored part limit refused: %v", err)
						}
						continue
					}
					if scenario == "concurrent descriptors" {
						start := make(chan struct{})
						results := make(chan error, 2)
						var workers sync.WaitGroup
						for sequence := int32(0); sequence < 2; sequence++ {
							workers.Go(func() {
								<-start
								value := object
								value.Sequence = sequence
								value.ID = fmt.Sprintf("pending_%d", sequence)
								results <- put(value)
							})
						}
						close(start)
						workers.Wait()
						close(results)
						admitted, rejected := 0, 0
						for err := range results {
							if err == nil {
								admitted++
							} else if errors.Is(err, artifact.ErrQuota) {
								rejected++
							} else {
								t.Fatal(err)
							}
						}
						if admitted != 1 || rejected != 1 {
							t.Fatalf("reservation over-authorized concurrent descriptors: admitted=%d rejected=%d", admitted, rejected)
						}
						continue
					}
					if err := put(object); err != nil {
						t.Fatal(err)
					}
					second := object
					second.Sequence = 1
					second.ID = "pending_1"
					if err := put(second); !errors.Is(err, artifact.ErrQuota) {
						t.Fatalf("two 800-byte descriptors authorized under 1000-byte reservation: %v", err)
					}
					second.Size = 200
					if err := put(second); err != nil {
						t.Fatalf("remaining reserved bytes refused: %v", err)
					}
					third := object
					third.Sequence = 2
					third.ID = "pending_2"
					third.Size = 1
					if err := put(third); !errors.Is(err, artifact.ErrQuota) {
						t.Fatalf("uncertain writes lost pending reservation: %v", err)
					}
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						_, err := w.DiscardCatalogObject(t.Context(), object, now, "")
						return err
					}); !errors.Is(err, artifact.ErrInvalid) {
						t.Fatalf("unreceipted discard released reservation: %v", err)
					}
					if err := put(third); !errors.Is(err, artifact.ErrQuota) {
						t.Fatalf("failed discard changed pending bytes: %v", err)
					}
					if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
						_, err := w.DiscardCatalogObject(t.Context(), object, now, "objects/definite-rejection")
						return err
					}); err != nil {
						t.Fatal(err)
					}
					third.Size = 800
					if err := put(third); err != nil {
						t.Fatalf("definite discard did not release descriptor admission: %v", err)
					}
				}
			}
			if scenario == "outstanding descriptors" {
				if err := repository.Close(); err != nil {
					t.Fatal(err)
				}
				repository, _ = repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
				for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
					for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
						err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
							_, err := w.PutCatalogObject(t.Context(), postgres.CatalogObject{OrganizationID: org, ProjectID: project, ServiceID: fmt.Sprintf("service_%032x", 1), UploadID: "upload", Sequence: 3, ID: "after_reopen", MediaType: "text/plain", Size: 1, Sha256: artifact.Digest([]byte("after_reopen"))}, now)
							return err
						})
						if !errors.Is(err, artifact.ErrQuota) {
							t.Fatalf("reopen lost pending descriptor reservation: %v", err)
						}
					}
				}
			}
		})
	}
}
