package workload

import (
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         workflowconfig.Config
		wantClass   Class
		wantSignals Signals
	}{
		{
			name:      "command validation gate",
			cfg:       workflowconfig.Config{Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"}},
			wantClass: ClassLocalHeavy,
			wantSignals: Signals{
				LocalGate: true,
			},
		},
		{
			name:      "validator",
			cfg:       workflowconfig.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}},
			wantClass: ClassLocalHeavy,
			wantSignals: Signals{
				LocalGate: true,
			},
		},
		{
			name:      "CI trigger label",
			cfg:       workflowconfig.Config{Gate: gate.Config{CITriggerLabel: "ci:ready"}},
			wantClass: ClassLocalHeavy,
			wantSignals: Signals{
				CITrigger: true,
			},
		},
		{
			name:      "required CI checks",
			cfg:       workflowconfig.Config{Gate: gate.Config{RequiredStatusChecks: []string{"test"}}},
			wantClass: ClassLocalHeavy,
			wantSignals: Signals{
				CITrigger: true,
			},
		},
		{
			name:      "no local gate or CI trigger",
			cfg:       workflowconfig.Config{Gate: gate.Config{Kind: gate.KindArtifact}},
			wantClass: ClassCloudOnly,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotClass, gotSignals := Classify(tt.cfg)
			if gotClass != tt.wantClass || gotSignals != tt.wantSignals {
				t.Fatalf("Classify() = %q, %#v, want %q, %#v", gotClass, gotSignals, tt.wantClass, tt.wantSignals)
			}
		})
	}
}
