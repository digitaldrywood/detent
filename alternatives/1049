package global

import "testing"

func TestLocalIntakePolicy(t *testing.T) {
	for _, test := range []struct {
		name            string
		global, project *bool
		want            bool
	}{
		{name: "default", want: true},
		{name: "global disabled", global: new(false)},
		{name: "project disabled", project: new(false)},
		{name: "unrelated project override", global: new(false), project: new(true), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := Project{LocalIntakeEnabled: test.project, GlobalLocalIntakeEnabled: test.global}
			if project.LocalIntakeOn() != test.want {
				t.Fatalf("effective intake = %t", project.LocalIntakeOn())
			}
		})
	}
	for _, value := range []any{"false", 0, []any{false}} {
		if _, err := buildSettings(map[string]any{"local_intake_enabled": value}, defaultOptions()); err == nil {
			t.Fatalf("invalid intake policy accepted: %v", value)
		}
	}
}
