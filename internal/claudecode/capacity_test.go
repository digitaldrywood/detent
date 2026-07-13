package claudecode

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAgentBackendClassifyCapacityError(t *testing.T) {
	t.Parallel()

	backend := &AgentBackend{}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "anthropic rate limit without reset", err: errors.New("claude turn failed: rate_limit_error")},
		{name: "vertex quota without reset", err: errors.New("claude turn failed: RESOURCE_EXHAUSTED")},
		{name: "anthropic overload", err: errors.New("claude turn failed: overloaded_error"), want: true},
		{name: "http overload", err: errors.New("claude turn failed: HTTP 529"), want: true},
		{name: "max turns", err: errors.New("claude turn failed: error_max_turns")},
		{name: "result message", err: finalTurnError(context.Background(), turnState{sawResult: true, resultIsError: true, resultSubtype: "error_during_execution", resultText: "You've hit your limit. Try again at 9:39 PM"}, nil, ""), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, ok := backend.ClassifyCapacityError(tt.err, nil, time.Now())
			if ok != tt.want {
				t.Fatalf("ClassifyCapacityError() ok = %v, want %v", ok, tt.want)
			}
		})
	}
}
