package executionrecords_test

import (
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptMetadataBoundsTenantPage(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		for i := range 3 {
			claim := pgstoretest.LeaseClaim(1)
			claim.LeaseID, claim.SessionID = fmt.Sprintf("lease_%d", i), fmt.Sprintf("session_%d", i)
			lease := pgstoretest.ClaimLease(t, repository, org, claim)
			event := pgstoretest.AttemptEvent(lease)
			ref, _, err := attachment.PrepareArchive(string(org), 1, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			event.PayloadReference, event.PayloadObjectKey = ref, ref.Key
			pgstoretest.RecordLeaseAttempt(t, repository, org, event)
			if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events
			(organization_id,project_id,work_item_id,id,sequence,schema_version,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			SELECT organization_id,'prj_1','wi_1','event_'||id,row_number() OVER (ORDER BY fencing_token),1,id,updated_at,
			'orgs/'||organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
			FROM native_attempts WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		checkpoint, _, err := attachment.PrepareArchive(string(org), 1, strings.NewReader(`{"checkpoint":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "checkpoint", Reference: checkpoint, CreatedAt: pgstoretest.HistoryAt()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		artifacts, _, err := attachment.PrepareArchive(string(org), 1, strings.NewReader(`["retained-artifact"]`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "artifacts", Reference: artifacts, CreatedAt: pgstoretest.HistoryAt()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_object_key=$2, artifacts_object_key=$3 WHERE organization_id=$1`, org, checkpoint.Key, artifacts.Key); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, size := range []int{2, 3} {
			t.Run(fmt.Sprintf("%s/%d", org, size), func(t *testing.T) {
				if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{}, size)
					if err != nil {
						return err
					}
					reader, ok := r.(tenantstore.AttemptMetadataReader)
					if !ok {
						t.Fatal("attempt metadata reader is unavailable")
					}
					metadata, err := reader.AttemptMetadata(t.Context(), "prj_1", "wi_1", page.Items, true)
					if err != nil {
						return err
					}
					if len(metadata.Leases) != size || len(metadata.Events) != size || len(metadata.Archives) != 3 || len(metadata.Attachments) != 0 {
						t.Fatalf("metadata shape: leases=%d events=%d archives=%d attachments=%d", len(metadata.Leases), len(metadata.Events), len(metadata.Archives), len(metadata.Attachments))
					}
					for _, attempt := range page.Items {
						if attempt.CheckpointObjectKey == nil || metadata.Archives[*attempt.CheckpointObjectKey].OrganizationID != org {
							t.Fatal("attempt snapshot omitted its tenant checkpoint metadata")
						}
						if attempt.ArtifactsObjectKey == nil || metadata.Archives[*attempt.ArtifactsObjectKey].OrganizationID != org || metadata.Archives[*attempt.ArtifactsObjectKey].Reference.Key != *attempt.ArtifactsObjectKey {
							t.Fatal("attempt snapshot omitted its tenant retained artifact metadata")
						}
						lease, event := metadata.Leases[attempt.LeaseID], metadata.Events[attempt.ID]
						if lease.OrganizationID != org || lease.FencingToken != attempt.FencingToken || event.OrganizationID != org || event.AttemptID != attempt.ID || event.WorkItemID != "wi_1" {
							t.Fatalf("attempt metadata crossed identity: %+v %+v", lease, event)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
