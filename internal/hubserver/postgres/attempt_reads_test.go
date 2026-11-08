package postgres_test

import (
	"context"
	"fmt"

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
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		for i := range 50 {
			claim := leaseClaim(1)
			claim.LeaseID, claim.SessionID = fmt.Sprintf("lease_%d", i), fmt.Sprintf("session_%d", i)
			lease := claimLease(t, repository, org, claim)
			event := attemptEvent(lease)
			ref, _, err := attachment.PrepareArchive(string(org), 1, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			event.PayloadReference, event.PayloadObjectKey = ref, ref.Key
			recordLeaseAttempt(t, repository, org, event)
			if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), leaseFence(lease), "finished")
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
			_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "checkpoint", Reference: checkpoint, CreatedAt: historyAt()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_object_key=$2 WHERE organization_id=$1`, org, checkpoint.Key); err != nil {
			t.Fatal(err)
		}
	}
	repository, counts := attemptCountedRepository(t, repository, pool)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, size := range []int{2, 50} {
			t.Run(fmt.Sprintf("%s/%d", org, size), func(t *testing.T) {
				counts.exchanges.Store(0)
				counts.statements.Store(0)
				if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{}, size)
					if err != nil {
						return err
					}
					reader, ok := r.(tenantstore.AttemptMetadataReader)
					if !ok {
						t.Fatal("attempt metadata still requires per-attempt queries")
					}
					metadata, err := reader.AttemptMetadata(t.Context(), "prj_1", "wi_1", page.Items, true)
					if err != nil {
						return err
					}
					if len(metadata.Leases) != size || len(metadata.Events) != size || len(metadata.Archives) != 2 || len(metadata.Attachments) != 0 {
						t.Fatalf("metadata shape: leases=%d events=%d archives=%d attachments=%d", len(metadata.Leases), len(metadata.Events), len(metadata.Archives), len(metadata.Attachments))
					}
					for _, attempt := range page.Items {
						if attempt.CheckpointObjectKey == nil || metadata.Archives[*attempt.CheckpointObjectKey].OrganizationID != org {
							t.Fatal("attempt snapshot omitted its tenant checkpoint metadata")
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
				if exchanges, statements := counts.exchanges.Load(), counts.statements.Load(); exchanges != 4 || statements != 9 {
					t.Fatalf("metadata snapshot: exchanges=%d statements=%d, want 4/9", exchanges, statements)
				}
				t.Logf("attempts=%d exchanges=%d statements=%d", size, counts.exchanges.Load(), counts.statements.Load())
				counts.exchanges.Store(0)
				counts.statements.Store(0)
				if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{}, size)
					if err != nil {
						return err
					}
					for _, attempt := range page.Items {
						if _, err := r.Lease(t.Context(), attempt.LeaseID); err != nil {
							return err
						}
						if _, err := r.LatestRunEvent(t.Context(), "prj_1", attempt.ID); err != nil {
							return err
						}
						if _, err := r.(interface {
							ArchiveObjectByKey(context.Context, string) (artifactstore.ArchiveObject, error)
						}).ArchiveObjectByKey(t.Context(), attempt.PayloadObjectKey); err != nil {
							return err
						}
						if _, err := r.AttachmentEvidence(t.Context(), "prj_1", "wi_1", attempt.ID, 201); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if exchanges, statements := counts.exchanges.Load(), counts.statements.Load(); exchanges != int64(3+4*size) || statements != int64(4+4*size) {
					t.Fatalf("per-attempt baseline: exchanges=%d statements=%d", exchanges, statements)
				}
				t.Logf("per-attempt baseline: attempts=%d exchanges=%d statements=%d", size, counts.exchanges.Load(), counts.statements.Load())
			})
		}
	}
}
