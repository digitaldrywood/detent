package postgres_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthAttemptSourcesUseBoundedTenantBatches(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	ids := map[tracker.OrganizationID]map[tracker.ProjectID][]string{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		ids[org] = map[tracker.ProjectID][]string{}
		for projectNumber := int64(1); projectNumber <= 2; projectNumber++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", projectNumber))
			for i := range 50 {
				claim := leaseClaim(projectNumber)
				claim.LeaseID, claim.SessionID = fmt.Sprintf("health_%s_%d_%d", org, projectNumber, i), fmt.Sprintf("health_session_%s_%d_%d", org, projectNumber, i)
				lease := claimLease(t, repository, org, claim)
				event := attemptEvent(lease)
				ref, _, err := attachment.PrepareArchive(string(org), 1, strings.NewReader(fmt.Sprintf(`{"project":%d,"position":%d}`, projectNumber, i)))
				if err != nil {
					t.Fatal(err)
				}
				event.PayloadReference, event.PayloadObjectKey = ref, ref.Key
				recordLeaseAttempt(t, repository, org, event)
				ids[org][project] = append(ids[org][project], event.ID)
				if err := repository.WriteLeases(t.Context(), org, corestore.Background, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), leaseFence(lease), "finished")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,schema_version,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT a.organization_id,i.project_id,i.native_id,'event_'||a.id,row_number() OVER(PARTITION BY i.project_id ORDER BY a.id),1,a.id,a.updated_at,
 'orgs/'||a.organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
	}
	repository, counts := attemptCountedRepository(t, repository, pool)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_1", "prj_2"} {
			for _, size := range []int{2, 50} {
				counts.exchanges.Store(0)
				counts.statements.Store(0)
				if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
					sources, err := r.(healthstore.AttemptSourceReader).HealthAttemptSources(t.Context(), project, ids[org][project][:size])
					if err != nil {
						return err
					}
					if len(sources.Attempts) != size || len(sources.Metadata.Leases) != size || len(sources.Metadata.Events) != size || len(sources.Metadata.Archives) != size || len(sources.Metadata.Attachments) != 0 {
						t.Fatalf("organization=%s project=%s size=%d wrong source counts: attempts=%d leases=%d events=%d archives=%d attachments=%d", org, project, size, len(sources.Attempts), len(sources.Metadata.Leases), len(sources.Metadata.Events), len(sources.Metadata.Archives), len(sources.Metadata.Attachments))
					}
					for _, id := range ids[org][project][:size] {
						attempt := sources.Attempts[id]
						lease, event, object := sources.Metadata.Leases[attempt.LeaseID], sources.Metadata.Events[id], sources.Metadata.Archives[attempt.PayloadObjectKey]
						if attempt.OrganizationID != org || lease.OrganizationID != org || lease.FencingToken != attempt.FencingToken || event.OrganizationID != org || event.ProjectID != project || event.AttemptID != id || object.OrganizationID != org || object.Reference.Key != attempt.PayloadObjectKey {
							t.Fatalf("organization=%s project=%s attempt=%s crossed source identity", org, project, id)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if statements, exchanges := counts.statements.Load(), counts.exchanges.Load(); statements != 8 || exchanges != 4 {
					t.Fatalf("organization=%s project=%s size=%d source statements/exchanges=%d/%d want 8/4", org, project, size, statements, exchanges)
				}
			}
		}
		if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
			owner := r.(healthstore.AttemptSourceReader)
			foreign := tracker.OrganizationID("org_first")
			if org == foreign {
				foreign = "org_second"
			}
			for _, requested := range [][]string{ids[foreign]["prj_1"], ids[org]["prj_2"]} {
				sources, err := owner.HealthAttemptSources(t.Context(), "prj_1", requested)
				if err != nil {
					return err
				}
				if len(sources.Attempts) != 0 || len(sources.Metadata.Leases) != 0 || len(sources.Metadata.Events) != 0 || len(sources.Metadata.Archives) != 0 || len(sources.Metadata.Attachments) != 0 {
					t.Fatal("health attempt sources exposed foreign scope")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
