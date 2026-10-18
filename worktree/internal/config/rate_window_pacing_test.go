package config

import (
	"strings"
	"testing"
)

func TestRateWindowPacingConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		yaml           string
		want           RateWindowPacing
		wantConfigured bool
		wantError      string
	}{
		{
			name: "default proportional",
			yaml: "",
			want: DefaultRateWindowPacing(),
		},
		{
			name:           "off",
			yaml:           "agent:\n  rate_window_pacing:\n    mode: off\n",
			want:           RateWindowPacing{Mode: RateWindowPacingOff, FloorPercent: DefaultRateWindowPacingFloorPercent, StaleAfterSeconds: DefaultRateWindowPacingStaleAfterSeconds},
			wantConfigured: true,
		},
		{
			name:           "floor",
			yaml:           "agent:\n  rate_window_pacing:\n    mode: floor\n    floor_percent: 35\n    stale_after_seconds: 600\n",
			want:           RateWindowPacing{Mode: RateWindowPacingFloor, FloorPercent: 35, StaleAfterSeconds: 600},
			wantConfigured: true,
		},
		{
			name:      "invalid mode",
			yaml:      "agent:\n  rate_window_pacing:\n    mode: burst\n",
			wantError: "agent.rate_window_pacing.mode must be one of proportional, off, floor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow, err := ParseWorkflow([]byte("---\ntracker:\n  kind: memory\n" + tt.yaml + "---\n"))
			if err == nil {
				err = workflow.Config.Validate()
			}
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("ParseWorkflow() error = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			if got := workflow.Config.Agent.RateWindowPacing; got != tt.want {
				t.Fatalf("RateWindowPacing = %#v, want %#v", got, tt.want)
			}
			if got := workflow.Config.RateWindowPacingConfigured(); got != tt.wantConfigured {
				t.Fatalf("RateWindowPacingConfigured() = %t, want %t", got, tt.wantConfigured)
			}
		})
	}
}
