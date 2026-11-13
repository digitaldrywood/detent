package runnerfleet_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type runnerCapacityChange struct {
	runnerauth.CapacityRequest
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

type runnerRoutingRequest struct {
	runnerauth.RoutingChange
	IdempotencyKey string                   `json:"idempotency_key,omitempty"`
	IsolationTier  *string                  `json:"isolation_tier"`
	HostServices   *[]string                `json:"host_services"`
	Availability   *runnerauth.Availability `json:"availability"`
}

func readRunnerRouting(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, runner string) (runnerauth.Runner, error) {
	t.Helper()
	response := h.Request(t, http.MethodGet, hubfixture.OrganizationBase(organization)+"/runners/"+runner+"/routing", hubfixture.AdminToken, nil)
	if response.Code != http.StatusOK {
		return runnerauth.Runner{}, errors.New(strings.TrimSpace(response.Body.String()))
	}
	var result runnerauth.Runner
	hubfixture.Decode(t, response, &result)
	return result, nil
}

func seedStartupOrganization(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID) {
	t.Helper()
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example-" + strings.TrimPrefix(string(organization), "org_"), CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization), AllocationGeneration: 1})
	}); err != nil {
		t.Fatal(err)
	}
}

func hostHistory(t *testing.T, f hubfixture.Native, organization tracker.OrganizationID, runner string, from, to time.Time) ([]runneradvice.HostHour, error) {
	t.Helper()
	return runneradvice.New(runneradvice.Deps{Repository: f.Hub.Config.TenantRepository, Now: f.Hub.Config.Clock}).HostHistory(t.Context(), organization, runner, from, to)
}

func readRunnerRanks(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, runner string) (map[tracker.ProjectID]int, error) {
	t.Helper()
	var ranks map[tracker.ProjectID]int
	err := h.Config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		identity, err := r.RunnerIdentity(t.Context(), runner)
		ranks = identity.ProjectRanks
		return err
	})
	return ranks, err
}
