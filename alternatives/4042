package shutdown

import "testing"

func TestMachineTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		events     []Event
		wantPhase  Phase
		wantResult Result
	}{
		{
			name:       "request enters drain",
			events:     []Event{EventDrainRequested},
			wantPhase:  PhaseDraining,
			wantResult: ResultNone,
		},
		{
			name:       "drained completes gracefully",
			events:     []Event{EventDrainRequested, EventDrained},
			wantPhase:  PhaseComplete,
			wantResult: ResultGraceful,
		},
		{
			name:       "force while draining completes forced",
			events:     []Event{EventDrainRequested, EventForceRequested},
			wantPhase:  PhaseComplete,
			wantResult: ResultForced,
		},
		{
			name:       "timeout while draining completes timeout",
			events:     []Event{EventDrainRequested, EventDrainTimedOut},
			wantPhase:  PhaseComplete,
			wantResult: ResultTimeout,
		},
		{
			name:       "force from running completes forced",
			events:     []Event{EventForceRequested},
			wantPhase:  PhaseComplete,
			wantResult: ResultForced,
		},
		{
			name:       "completed machine ignores later events",
			events:     []Event{EventDrainRequested, EventDrained, EventForceRequested},
			wantPhase:  PhaseComplete,
			wantResult: ResultGraceful,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machine := NewMachine()
			for _, event := range tt.events {
				machine = machine.Apply(event)
			}

			if machine.Phase != tt.wantPhase {
				t.Fatalf("Phase = %q, want %q", machine.Phase, tt.wantPhase)
			}
			if machine.Result != tt.wantResult {
				t.Fatalf("Result = %q, want %q", machine.Result, tt.wantResult)
			}
		})
	}
}
