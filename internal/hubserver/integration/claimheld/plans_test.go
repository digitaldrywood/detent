package claimheld_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (f heldFixture) candidatePage(t *testing.T) claimstore.CandidatePage {
	t.Helper()
	query := claimstore.CandidateQuery{ProjectID: f.Project.ID, Scope: string(f.Project.ID), States: []string{"todo"}, RunnerSelection: true, AvailableAt: f.Hub.Now(), Limit: 100}
	var page claimstore.CandidatePage
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		page, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestHeldCandidatesAreNotClaimable(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	f := newHeldProject(t, h, "", hubfixture.States())
	for index := range 2 {
		f.hold(t, f.create(t, fmt.Sprintf("Held %d", index), 0))
	}
	f.capacity(t)
	page := f.candidatePage(t)
	if len(page.Items) != 0 {
		t.Fatalf("all-held candidate page=%+v", page.Items)
	}
	response := f.claim(t, "", "all-held")
	if response.Code != http.StatusConflict {
		t.Fatalf("all-held scan status=%d: %s", response.Code, response.Body.Bytes())
	}

	tail := f.create(t, "Eligible tail", 3)
	page = f.candidatePage(t)
	if len(page.Items) != 1 || page.Items[0].WorkItemID != tail.WorkItemID {
		t.Fatalf("candidate page=%+v, want only the eligible tail", page.Items)
	}
}
