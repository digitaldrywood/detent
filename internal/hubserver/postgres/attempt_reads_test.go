package postgres_test

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptMetadataBoundsTenantPage(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		for i := range 50 {
			claim := leaseClaim(1)
			claim.LeaseID, claim.SessionID = fmt.Sprintf("lease_%d", i), fmt.Sprintf("session_%d", i)
			lease := claimLease(t, repository, org, claim)
			recordLeaseAttempt(t, repository, org, attemptEvent(lease))
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
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, size := range []int{2, 50} {
			t.Run(fmt.Sprintf("%s/%d", org, size), func(t *testing.T) {
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
					if len(metadata.Leases) != size || len(metadata.Events) != size || len(metadata.Archives) != 0 || len(metadata.Attachments) != 0 {
						t.Fatalf("metadata shape: leases=%d events=%d archives=%d attachments=%d", len(metadata.Leases), len(metadata.Events), len(metadata.Archives), len(metadata.Attachments))
					}
					for _, attempt := range page.Items {
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
