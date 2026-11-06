package global

import (
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestBuildGlobalRateWindowPacing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     any
		want      workflowconfig.RateWindowPacing
		wantError string
	}{
		{name: "default", want: workflowconfig.DefaultRateWindowPacing()},
		{name: "off", value: map[string]any{"mode": "off"}, want: workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}.Normalized()},
		{name: "floor", value: map[string]any{"mode": "floor", "floor_percent": 30, "stale_after_seconds": 600}, want: workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingFloor, FloorPercent: 30, StaleAfterSeconds: 600}},
		{name: "invalid", value: map[string]any{"mode": "burst"}, wantError: "mode must be one of proportional, off, floor"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attrs := map[string]any{"max_concurrent_agents": 8, "scheduling": SchedulingWeighted}
			if tt.value != nil {
				attrs["rate_window_pacing"] = tt.value
			}
			if problems := rateWindowPacingErrors(tt.value, "global.rate_window_pacing"); len(problems) > 0 {
				if tt.wantError == "" || !strings.Contains(strings.Join(problems, "; "), tt.wantError) {
					t.Fatalf("rateWindowPacingErrors() = %v", problems)
				}
				return
			}
			settings, err := buildSettings(attrs, defaultOptions())
			if err != nil {
				t.Fatalf("buildSettings() error = %v", err)
			}
			if settings.RateWindowPacing != tt.want {
				t.Fatalf("RateWindowPacing = %#v, want %#v", settings.RateWindowPacing, tt.want)
			}
		})
	}
}
