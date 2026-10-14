package project

import (
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestEffectiveRateWindowPacingProjectOverride(t *testing.T) {
	t.Parallel()

	global := workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}.Normalized()
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "global applies when project omits setting", yaml: "", want: workflowconfig.RateWindowPacingOff},
		{name: "project override wins", yaml: "agent:\n  rate_window_pacing:\n    mode: proportional\n", want: workflowconfig.RateWindowPacingProportional},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: memory\n" + tt.yaml + "---\n"))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			project := globalconfig.Project{GlobalRateWindowPacing: global}
			if got := effectiveRateWindowPacing(project, workflow.Config).Mode; got != tt.want {
				t.Fatalf("effective mode = %q, want %q", got, tt.want)
			}
		})
	}
}
