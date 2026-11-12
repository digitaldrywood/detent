package workpage_test

import (
	"bytes"
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"net/http"
	"net/url"
	"testing"
)

func TestNativeOrganizationCursorPaging(t *testing.T) {
	h := openHub(t)
	service := h.service
	var previous string
	for _, test := range []struct {
		name   string
		legacy bool
	}{{"First example", false}, {"Second example", false}, {"First legacy", true}, {"Second legacy", true}} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations", h.token, map[string]any{"name": test.name})
			requireNativeStatus(t, response, http.StatusCreated)
			var organization organization
			decodeHubResponse(t, response, &organization)
			f := h.fixture(t, organization.ID)
			if test.legacy {
				if _, err := h.admin.ExecContext(t.Context(), `DELETE FROM organization_secrets WHERE organization_id=$1 AND kind=$2`, organization.ID, accessstore.NativeCursorKeyKind); err != nil {
					t.Fatal(err)
				}
			}
			item := f.create(t, "First item")
			f.create(t, "Second item")
			comments := f.base + "/work-items/" + string(item.WorkItemID) + "/comments"
			for _, body := range []string{"First comment", "Second comment"} {
				response := performHubAPIRequest(t, service, http.MethodPost, comments, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: body}, Body: body})
				requireNativeStatus(t, response, http.StatusOK)
			}
			h.refreshBoard(t, organization.ID)
			for _, path := range []string{f.base + "/work-items", comments, f.base + "/work-items/" + string(item.WorkItemID) + "/history"} {
				response := performHubAPIRequest(t, service, http.MethodGet, path+"?limit=1", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[json.RawMessage]
				decodeHubResponse(t, response, &page)
				if len(page.Items) != 1 || page.NextCursor == "" {
					t.Fatalf("first page at %s: items=%d cursor present=%v", path, len(page.Items), page.NextCursor != "")
				}
				response = performHubAPIRequest(t, service, http.MethodGet, path+"?limit=1&cursor="+url.QueryEscape(page.NextCursor), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var next tracker.Page[json.RawMessage]
				decodeHubResponse(t, response, &next)
				if len(next.Items) != 1 {
					t.Fatalf("second page at %s: items=%d", path, len(next.Items))
				}
				if bytes.Equal(page.Items[0], next.Items[0]) {
					t.Fatalf("cursor repeated the first record at %s", path)
				}
				if path == f.base+"/work-items" {
					if previous != "" {
						response = performHubAPIRequest(t, service, http.MethodGet, path+"?cursor="+url.QueryEscape(previous), f.token, nil)
						requireNativeStatus(t, response, http.StatusUnprocessableEntity)
					}
					previous = page.NextCursor
				}
			}
		})
	}
}
