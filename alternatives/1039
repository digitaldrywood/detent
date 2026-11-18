package efficiency

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOTLPExporterEmitsLifecycleSpans(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("collector path = %q, want /v1/traces", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Errorf("Authorization = %q, want fixture header", r.Header.Get("Authorization"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		requests <- payload
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	exporter, err := NewLifecycleExporter(ExporterConfig{
		Endpoint:    collector.URL,
		Headers:     map[string]string{"Authorization": "Bearer fixture"},
		ServiceName: "detent-test",
	})
	if err != nil {
		t.Fatalf("NewLifecycleExporter() error = %v", err)
	}
	completedAt := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	if err := exporter.ExportLifecycle(context.Background(), Receipt{
		ProjectID:         "detent",
		IssueID:           "issue-1205",
		Identifier:        "digitaldrywood/detent#1205",
		FirstDispatchedAt: completedAt.Add(-10 * time.Minute),
		CompletedAt:       completedAt,
		WallSeconds:       600,
		WorkingSeconds:    300,
		GateWaitSeconds:   120,
		MergeTrainSeconds: 60,
		ParkedSeconds:     30,
		TotalTokens:       250000,
		InputTokens:       200000,
		CachedInputTokens: 190000,
		EstimatedCostUSD:  1.25,
	}); err != nil {
		t.Fatalf("ExportLifecycle() error = %v", err)
	}

	payload := <-requests
	resourceSpans := payload["resourceSpans"].([]any)
	scopeSpans := resourceSpans[0].(map[string]any)["scopeSpans"].([]any)
	spans := scopeSpans[0].(map[string]any)["spans"].([]any)
	wantNames := []string{"detent.dispatch", "detent.session", "detent.gate", "detent.merge"}
	if len(spans) != len(wantNames) {
		t.Fatalf("span count = %d, want %d", len(spans), len(wantNames))
	}
	for index, want := range wantNames {
		span := spans[index].(map[string]any)
		if span["name"] != want {
			t.Fatalf("span %d name = %v, want %q", index, span["name"], want)
		}
		if index > 0 && span["parentSpanId"] == "" {
			t.Fatalf("span %d parentSpanId is empty", index)
		}
	}
}

func TestDisabledExporterIsInert(t *testing.T) {
	t.Parallel()

	exporter, err := NewLifecycleExporter(ExporterConfig{})
	if err != nil {
		t.Fatalf("NewLifecycleExporter() error = %v", err)
	}
	if err := exporter.ExportLifecycle(context.Background(), Receipt{}); err != nil {
		t.Fatalf("ExportLifecycle() error = %v", err)
	}
}
