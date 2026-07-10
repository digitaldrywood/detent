package intake

import "testing"

func TestDefaultWebhookFactoryNormalizesSupportedPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		kind        string
		payload     string
		wantSummary string
		wantID      string
	}{
		{name: "generic", kind: KindWebhook, payload: `{"summary":"Disk full","fingerprint":"disk-1"}`, wantSummary: "Disk full", wantID: "disk-1"},
		{name: "sentry", kind: KindSentry, payload: `{"data":{"event":{"title":"panic in worker","fingerprint":["worker-panic"],"level":"error"}}}`, wantSummary: "panic in worker", wantID: `["worker-panic"]`},
		{name: "datadog", kind: KindDatadog, payload: `{"alert_title":"High latency","alert_id":"alert-9"}`, wantSummary: "High latency", wantID: "alert-9"},
		{name: "slack", kind: KindSlack, payload: `{"event_id":"Ev123","event":{"text":"@detent investigate"}}`, wantSummary: "@detent investigate", wantID: "Ev123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, err := DefaultWebhookFactory().New(tt.kind)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			event, err := adapter.Decode([]byte(tt.payload))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if event.Summary != tt.wantSummary || event.Fingerprint != tt.wantID {
				t.Fatalf("event = %#v, want summary %q fingerprint %q", event, tt.wantSummary, tt.wantID)
			}
			if tt.kind == KindSentry && event.Fields["level"] != "error" {
				t.Fatalf("event level = %q, want error", event.Fields["level"])
			}
		})
	}
}
