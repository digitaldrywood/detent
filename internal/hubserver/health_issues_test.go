package hubserver

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func addHealthBacklog(t *testing.T, f nativeFixture) {
	t.Helper()
	states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
	for i := range states {
		if states[i].Name == "Todo" {
			states[i].Transitions = append(states[i].Transitions, "Backlog")
		}
	}
	setHealthProjectStates(t, f, states)
}

func setHealthProjectStates(t *testing.T, f nativeFixture, states []tracker.NativeState) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.States = states
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
