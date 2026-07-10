package backendcapacity

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	wantReset := now.Add(44 * time.Minute)
	tests := []struct {
		name      string
		text      string
		fallback  *time.Time
		wantOK    bool
		wantReset time.Time
	}{
		{
			name:      "structured epoch reset",
			text:      `{"error":{"type":"usageLimitExceeded","resetAt":1783651140}}`,
			wantOK:    true,
			wantReset: time.Unix(1783651140, 0).UTC(),
		},
		{
			name:      "telemetry fallback",
			text:      `{"error":{"type":"usageLimitExceeded"}}`,
			fallback:  &wantReset,
			wantOK:    true,
			wantReset: wantReset,
		},
		{
			name:   "ordinary backend failure",
			text:   `{"error":{"type":"invalid_request_error"}}`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			details, ok := Classify(tt.text, tt.fallback, now, Rules{Kinds: []string{"usageLimitExceeded"}})
			if ok != tt.wantOK {
				t.Fatalf("Classify() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if details.ResetAt == nil || !details.ResetAt.Equal(tt.wantReset) {
				t.Fatalf("Classify() ResetAt = %v, want %v", details.ResetAt, tt.wantReset)
			}
		})
	}
}

func TestScopeHosted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "openai", want: true},
		{provider: "", want: true},
		{provider: "local_ollama", want: false},
		{provider: "ollama", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			t.Parallel()

			if got := (Scope{Provider: tt.provider}).Hosted(); got != tt.want {
				t.Fatalf("Hosted() = %v, want %v", got, tt.want)
			}
		})
	}
}
