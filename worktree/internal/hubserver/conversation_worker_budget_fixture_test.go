package hubserver

import (
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *conversationWorkerFixture) monthlyBudget(t *testing.T, organization bool, policy budget.MonthlyPolicy) {
	t.Helper()
	project := f.project.ID
	if organization {
		project = ""
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.BillingMonthlyPolicy(t.Context(), project)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.SetBillingMonthlyPolicy(t.Context(), billingstore.MonthlyPolicy{OrganizationID: f.project.OrganizationID, ProjectID: project, Policy: policy, Actor: f.principal, UpdatedAt: f.now}, current.Revision); err != nil {
			return err
		}
		return w.RecordBillingMonthlyLease(t.Context(), billingstore.MonthlyLease{OrganizationID: f.project.OrganizationID, LeaseID: f.lease.ID, RunnerAPI: true})
	}); err != nil {
		t.Fatal(err)
	}
}

func renewConversationFixtureLease(t *testing.T, f *conversationWorkerFixture, ttl int) {
	t.Helper()
	before := f.lease
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: int64(ttl)})
	requireNativeStatus(t, response, http.StatusOK)
	var renewed tracker.NativeLease
	decodeHubResponse(t, response, &renewed)
	if renewed.ID != before.ID || renewed.SessionID != before.SessionID || renewed.FencingToken != before.FencingToken || !renewed.ExpiresAt.After(before.ExpiresAt) {
		t.Fatalf("fixture renewal changed ownership or failed to extend expiry: %+v", renewed)
	}
}
