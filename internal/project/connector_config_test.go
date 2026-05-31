package project

import (
	"reflect"
	"testing"

	workflowconfig "github.com/digitaldrywood/symphony-go/internal/config"
)

func TestTrackerStateMapConvertsWorkflowMap(t *testing.T) {
	t.Parallel()

	got := trackerStateMap(workflowconfig.MapValue(map[string]any{
		"Cancelled": "Done",
		" ":         "Ignored",
		"Blocked":   12,
	}))
	want := map[string]string{"Cancelled": "Done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trackerStateMap() = %#v, want %#v", got, want)
	}

	if got := trackerStateMap(workflowconfig.StringValue("$STATE_MAP_JSON")); got != nil {
		t.Fatalf("trackerStateMap(string) = %#v, want nil", got)
	}
}

func TestTrackerPriorityMapConvertsWorkflowMap(t *testing.T) {
	t.Parallel()

	got := trackerPriorityMap(workflowconfig.MapValue(map[string]any{
		"P0":          1,
		"No priority": nil,
		" ":           2,
		"Pbad":        "1",
	}))
	wantP0 := 1
	want := map[string]*int{
		"P0":          &wantP0,
		"No priority": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trackerPriorityMap() = %#v, want %#v", got, want)
	}

	if got := trackerPriorityMap(workflowconfig.StringValue("$PRIORITY_MAP_JSON")); got != nil {
		t.Fatalf("trackerPriorityMap(string) = %#v, want nil", got)
	}
}
