package reportoptions_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestReportOptionsPopulationTooLarge(t *testing.T) {
	t.Parallel()
	h := hubfixture.NewHostedSecurity(t)
	owner := h.User(t, "owner", "owner", "owner@example.test", "", "")
	now := h.Now().UTC()
	if _, err := h.Admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name,archived,created_at,native_created_at,native_updated_at) VALUES ($1,900001,$2,'wi_options_labels',900001,'Example','human','actor_options','Todo',false,$3,$3,$3)`, h.Primary, h.Project, now); err != nil {
		t.Fatal(err)
	}
	addLabels := func(t *testing.T, from, to int) {
		t.Helper()
		if _, err := h.Admin.ExecContext(t.Context(), `INSERT INTO issue_labels(organization_id,project_id,work_item_id,label) SELECT $1,$2,'wi_options_labels','label_'||n FROM generate_series($3::int,$4::int) n`, h.Primary, h.Project, from, to); err != nil {
			t.Fatal(err)
		}
	}
	options := func(project string) string {
		return hubfixture.OrganizationBase(h.Primary) + "/reports/options?project=" + project + "&range=24h"
	}
	addLabels(t, 1, 200)
	response := h.Request(t, owner, http.MethodGet, options(string(h.Project)), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var facets struct {
		Labels []string `json:"labels"`
	}
	hubfixture.Decode(t, response, &facets)
	if len(facets.Labels) != 200 {
		t.Fatalf("labels within the facet limit = %d, want 200", len(facets.Labels))
	}

	addLabels(t, 201, 201)
	for _, project := range []string{string(h.Project), "all"} {
		response := h.Request(t, owner, http.MethodGet, options(project), nil)
		refusal := hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "report_population_too_large")
		if refusal.Message == "" {
			t.Fatalf("project %s refusal has no message: %s", project, response.Body.String())
		}
		var body map[string]json.RawMessage
		hubfixture.Decode(t, response, &body)
		for _, field := range []string{"labels", "filed_by", "runners", "models", "stages", "unavailable"} {
			if _, ok := body[field]; ok {
				t.Fatalf("project %s refusal carries partial option %q: %s", project, field, response.Body.String())
			}
		}
	}

	hubfixture.RequireStatus(t, h.Request(t, owner, http.MethodGet, options("prj_unseen"), nil), http.StatusNotFound)

	if _, err := h.Admin.ExecContext(t.Context(), `ALTER TABLE issue_labels RENAME TO issue_labels_unavailable`); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, h.Request(t, owner, http.MethodGet, options(string(h.Project)), nil), http.StatusServiceUnavailable)
}
