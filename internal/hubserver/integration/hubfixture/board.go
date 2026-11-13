package hubfixture

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (h *Hub) RefreshBoard(t *testing.T, organization tracker.OrganizationID) {
	t.Helper()
	var after tracker.NativeWorkItemID
	var attempted []tracker.NativeWorkItemID
	for {
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubserver.BoardMaintenanceBytes}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		next, processed, err := h.Service.MaintainBoardRecord(ctx, organization, after, attempted...)
		if err != nil {
			t.Fatal(err)
		}
		if !processed {
			return
		}
		after = next
		attempted = append(attempted, next)
	}
}

func ReadWorkItem(t *testing.T, p Native, id tracker.NativeWorkItemID, query string) tracker.NativeIssue {
	t.Helper()
	p.Hub.RefreshBoard(t, p.Project.OrganizationID)
	response := p.Hub.Request(t, http.MethodGet, p.Base+"/work-items/"+string(id)+query, p.Token, nil)
	RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	Decode(t, response, &issue)
	return issue
}
