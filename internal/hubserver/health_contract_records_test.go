package hubserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthContractReadCounter struct {
	tenantstore.Repository
	reads int
}

func (r *healthContractReadCounter) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads++
	return r.Repository.ReadTenant(ctx, org, workload, read)
}

func TestPostgresHealthContractCacheTracksRevisions(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	original := service.config.TenantRepository
	counter := &healthContractReadCounter{Repository: original}
	t.Cleanup(func() { service.config.TenantRepository = original })
	var cache healthContractCache
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		for _, title := range []string{"First candidate", "Second candidate"} {
			service.config.TenantRepository = original
			f := newNativeFixture(t, service, org, title)
			issue := f.create(t, title)
			project, descriptor, err := service.readHealthContractProject(t.Context(), org, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			service.config.TenantRepository = counter
			for _, step := range []string{"cold", "cached", "issue revision", "policy revision"} {
				service.config.TenantRepository = original
				if step == "issue revision" {
					title := "Changed candidate"
					response := performHubAPIRequest(t, service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "health-cache-title"}, ExpectedRevision: issue.Revision, Title: &title})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &issue)
				}
				if step == "policy revision" {
					descriptor.SourceRevision = strings.Repeat("b", 64)
					descriptor = descriptor.WithID()
				}
				service.config.TenantRepository = counter
				counter.reads = 0
				result, err := cache.readRecords(t.Context(), service, project, issue.WorkItemID, issue.Revision, descriptor)
				wait := result.wait
				if err != nil || wait != nil {
					t.Fatalf("step=%s wait=%+v err=%v", step, wait, err)
				}
				if (counter.reads == 0) != (step == "cached") {
					t.Fatalf("step=%s reads=%d", step, counter.reads)
				}
			}
		}
	}
}
