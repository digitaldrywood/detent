package hubserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerSyncChanges(t *testing.T) {
	t.Parallel()
	f := newRunnerPGFixture(t)
	binding := f.bindings[0]
	path := "/api/v2/organizations/" + string(f.organization) + "/runners/" + binding.RunnerID + "/sync"
	for _, id := range []tracker.ProjectID{"prj_two", "prj_three"} {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.organization, ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.organization, TokenID: "credential_1", ProjectID: id})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var versions json.RawMessage
	for _, step := range []string{"initial", "idle", "routing change", "idle again"} {
		if step == "routing change" {
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
				r, err := w.RunnerIdentity(t.Context(), binding.RunnerID)
				if err != nil {
					return err
				}
				r.DisplayName = "Updated"
				return w.UpdateRunnerRouting(t.Context(), r.Revision, r)
			}); err != nil {
				t.Fatal(err)
			}
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[0], map[string]any{"machine_id": binding.MachineID, "versions": versions})
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			Versions json.RawMessage `json:"versions"`
			Routing  *struct {
				Routing struct {
					DisplayName string `json:"display_name"`
				} `json:"routing"`
			} `json:"routing"`
			Projects map[string]json.RawMessage `json:"projects"`
			NoChange bool                       `json:"no_change"`
		}
		decodeHubResponse(t, response, &result)
		switch step {
		case "initial":
			if len(result.Projects) != 3 || result.Routing == nil || len(result.Versions) == 0 || result.NoChange {
				t.Fatalf("incomplete initial sync: %s", response.Body)
			}
		case "routing change":
			if result.NoChange || result.Routing == nil || result.Routing.Routing.DisplayName != "Updated" || len(result.Projects) != 0 {
				t.Fatalf("incorrect routing delta: %s", response.Body)
			}
		default:
			if !result.NoChange || result.Routing != nil || len(result.Projects) != 0 {
				t.Fatalf("unchanged sync returned deltas: %s", response.Body)
			}
		}
		if result.Versions != nil {
			versions = result.Versions
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], map[string]any{"machine_id": binding.MachineID}), http.StatusNotFound)
}
