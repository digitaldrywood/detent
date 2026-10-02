package hubclient

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalProjectCutoverReceipt(t *testing.T) {
	for _, scenario := range []string{"applied", "missing", "denied", "cross organization", "different project", "not native", "stale checkpoint", "different repository", "dry run", "blocked", "no mapping"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			checkpoint := strings.Repeat("a", 64)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if scenario == "denied" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v2/organizations/org_test/projects/prj_test") {
					t.Errorf("unexpected authority: %s %s", r.Method, r.URL.Path)
				}
				var response any
				if strings.HasSuffix(r.URL.Path, "/integration/cutover") {
					if scenario == "missing" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					applied, stamp, repository := scenario != "dry run", checkpoint, "acme/orders"
					if scenario == "stale checkpoint" {
						stamp = strings.Repeat("b", 64)
					}
					if scenario == "different repository" {
						repository = "acme/foreign"
					}
					blockers := []string{}
					if scenario == "blocked" {
						blockers = append(blockers, "handoff incomplete")
					}
					response = map[string]any{"applied": applied, "checkpoint": stamp, "blockers": blockers, "integration": map[string]string{"profile": "native", "intake": "disabled", "repository": repository}}
				} else {
					p := tracker.NativeProject{ID: "prj_test", OrganizationID: "org_test", Profile: "native"}
					if scenario == "cross organization" {
						p.OrganizationID = "org_foreign"
					}
					if scenario == "different project" {
						p.ID = "prj_foreign"
					}
					if scenario == "not native" {
						p.Profile = "github_compatible"
					}
					response = p
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			})
			client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "fixture" }, HTTPClient: providerHandlerClient(handler)})
			if err != nil {
				t.Fatal(err)
			}
			projects := map[string]tracker.ProjectID{"selected": "prj_test"}
			if scenario == "no mapping" {
				projects = nil
			}
			fleet, err := NewFleetClient(client, "org_test", projects)
			if err != nil {
				t.Fatal(err)
			}
			err = fleet.VerifyProjectCutover(t.Context(), "selected", "acme/orders", checkpoint)
			if (err == nil) != (scenario == "applied") || scenario == "no mapping" && calls != 0 {
				t.Fatalf("receipt error=%v calls=%d", err, calls)
			}
		})
	}
}
