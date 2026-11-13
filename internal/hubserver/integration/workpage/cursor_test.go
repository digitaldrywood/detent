package workpage_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeOrganizationCursorPaging(t *testing.T) {
	h := hubfixture.Open(t, hubfixture.Config{})
	service := h.Service
	var previous string
	for _, test := range []struct {
		name   string
		legacy bool
	}{{"First example", false}, {"Second example", false}, {"First legacy", true}, {"Second legacy", true}} {
		t.Run(test.name, func(t *testing.T) {
			response := hubfixture.Request(t, service.Handler(), http.MethodPost, "/api/v2/organizations", h.AdminToken(), map[string]any{"name": test.name})
			hubfixture.RequireStatus(t, response, http.StatusCreated)
			var organization organization
			hubfixture.Decode(t, response, &organization)
			f := hubfixture.NewNativeWith(t, h, organization.ID, hubtest.ProjectOptions{Name: "Example project"})
			if test.legacy {
				if _, err := h.Admin.ExecContext(t.Context(), `DELETE FROM organization_secrets WHERE organization_id=$1 AND kind=$2`, organization.ID, accessstore.NativeCursorKeyKind); err != nil {
					t.Fatal(err)
				}
			}
			item := f.Create(t, "First item")
			f.Create(t, "Second item")
			comments := f.Base + "/work-items/" + string(item.WorkItemID) + "/comments"
			for _, body := range []string{"First comment", "Second comment"} {
				response := hubfixture.Request(t, service.Handler(), http.MethodPost, comments, f.Token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: body}, Body: body})
				hubfixture.RequireStatus(t, response, http.StatusOK)
			}
			h.RefreshBoard(t, organization.ID)
			for _, path := range []string{f.Base + "/work-items", comments, f.Base + "/work-items/" + string(item.WorkItemID) + "/history"} {
				response := hubfixture.Request(t, service.Handler(), http.MethodGet, path+"?limit=1", f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page tracker.Page[json.RawMessage]
				hubfixture.Decode(t, response, &page)
				if len(page.Items) != 1 || page.NextCursor == "" {
					t.Fatalf("first page at %s: items=%d cursor present=%v", path, len(page.Items), page.NextCursor != "")
				}
				response = hubfixture.Request(t, service.Handler(), http.MethodGet, path+"?limit=1&cursor="+url.QueryEscape(page.NextCursor), f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var next tracker.Page[json.RawMessage]
				hubfixture.Decode(t, response, &next)
				if len(next.Items) != 1 {
					t.Fatalf("second page at %s: items=%d", path, len(next.Items))
				}
				if bytes.Equal(page.Items[0], next.Items[0]) {
					t.Fatalf("cursor repeated the first record at %s", path)
				}
				if path == f.Base+"/work-items" {
					if previous != "" {
						response = hubfixture.Request(t, service.Handler(), http.MethodGet, path+"?cursor="+url.QueryEscape(previous), f.Token, nil)
						hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
					}
					previous = page.NextCursor
				}
			}
		})
	}
}
