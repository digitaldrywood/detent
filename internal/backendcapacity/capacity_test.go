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

func TestClassifyTransientOverload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		wantKind string
		want     bool
	}{
		{name: "provider kind", text: `{"codexErrorInfo":"serverOverloaded"}`, wantKind: "serverOverloaded", want: true},
		{name: "model capacity phrase", text: "Selected model is at capacity", wantKind: "selectedmodelisatcapacity", want: true},
		{name: "http 529", text: "provider request failed: HTTP 529", wantKind: "http_529", want: true},
		{name: "status code 503", text: "provider request failed with status code 503", wantKind: "http_503", want: true},
		{name: "json status code", text: `{"status_code":502}`, wantKind: "http_502", want: true},
		{name: "unrelated number", text: "model context window is 500 tokens", want: false},
	}
	rules := Rules{
		Kinds:   []string{"serverOverloaded"},
		Phrases: []string{"selected model is at capacity"},
		HTTP5xx: true,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			details, ok := ClassifyTransientOverload(tt.text, rules)
			if ok != tt.want {
				t.Fatalf("ClassifyTransientOverload() ok = %v, want %v", ok, tt.want)
			}
			if !tt.want {
				return
			}
			if details.Type != ErrorTypeTransientOverload || details.Kind != tt.wantKind || details.ResetAt != nil {
				t.Fatalf("details = %#v, want transient overload kind %q without reset", details, tt.wantKind)
			}
		})
	}
}

func TestClassifyUsageLimitCanRequireReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 15, 0, 0, 0, time.UTC)
	rules := Rules{Kinds: []string{"RESOURCE_EXHAUSTED"}, RequireReset: true}
	if _, ok := Classify(`{"status":"RESOURCE_EXHAUSTED"}`, nil, now, rules); ok {
		t.Fatal("Classify() accepted reset-free usage limit")
	}
	resetAt := now.Add(time.Hour)
	details, ok := Classify(`{"status":"RESOURCE_EXHAUSTED"}`, &resetAt, now, rules)
	if !ok || details.Type != ErrorTypeUsageLimit || details.ResetAt == nil || !details.ResetAt.Equal(resetAt) {
		t.Fatalf("Classify() = %#v, %v, want reset-bearing usage limit", details, ok)
	}
}
