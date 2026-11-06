package hubserver

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIntakeFixture(t *testing.T) nativeFixture {
	t.Helper()
	f := newNativeFixture(t, nil, "", "health-intake")
	addHealthBacklog(t, f)
	states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true})
	for i := range states {
		if states[i].Name == "Todo" {
			states[i].Transitions = append(states[i].Transitions, "Backlog")
		}
	}
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", string(raw), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Cancelled','Cancelled',1,0,?,?)", f.project.ID, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	return f
}
