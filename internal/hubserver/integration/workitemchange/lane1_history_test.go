package workitemchange_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	serverapi "github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeDetailRefusesOversizedHistory(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewChangeInOrganization(t, nil)
	org := f.Project.OrganizationID
	content := eventstore.NewContent(f.Hub.Config.RunDetails)
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}
	rows := make([]eventstore.Evidence, corestore.MaxPageSize)
	for i := range rows {
		raw, err := json.Marshal(tracker.ChangeDiscussion{ID: fmt.Sprintf("discussion-%d", i), Body: "Example discussion", Actor: actor, CreatedAt: f.Hub.Now()})
		if err != nil {
			t.Fatal(err)
		}
		rows[i], err = content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "discussion"}, raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Hub.Repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, row := range rows {
			if _, err := w.AppendEvidence(t.Context(), row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Path+"/discussion", f.Token, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "boundary-discussion"}, Body: "Boundary discussion"}), http.StatusOK)
	response := f.Hub.Request(t, http.MethodGet, f.Path, f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
	var refusal serverapi.APIErrorResponse
	hubfixture.Decode(t, response, &refusal)
	if refusal.Code != "change_detail_too_large" || !strings.Contains(refusal.Message, "view=bounded") {
		t.Fatalf("oversized change detail refusal = %+v", refusal)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, f.Path+"?view=bounded", f.Token, nil), http.StatusOK)
}
