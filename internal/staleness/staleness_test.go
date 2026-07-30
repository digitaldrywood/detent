package staleness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	cfg := Config{
		Enabled: true,
		Lanes: []LaneThreshold{
			{State: "Human Review", Threshold: 48 * time.Hour, HumanGate: true},
			{State: "Blocked", Threshold: 24 * time.Hour},
		},
		NoCompletionThreshold:  12 * time.Hour,
		NoMergeThreshold:       6 * time.Hour,
		RepeatedDecisionCount:  3,
		RepeatedDecisionWindow: 24 * time.Hour,
	}

	tests := []struct {
		name      string
		input     Input
		wantKinds []string
		wantHuman bool
	}{
		{
			name: "lane threshold emits one human reminder",
			input: Input{
				ProjectID: "detent",
				Items: []Item{
					{ID: "41", State: "Human Review", EnteredAt: now.Add(-72 * time.Hour)},
					{ID: "41", State: "Human Review", EnteredAt: now.Add(-72 * time.Hour)},
				},
			},
			wantKinds: []string{KindLaneAging},
			wantHuman: true,
		},
		{
			name: "dispatchable queue without completions warns",
			input: Input{
				ProjectID:    "detent",
				Dispatchable: []Item{{ID: "1", EnteredAt: now.Add(-13 * time.Hour)}},
			},
			wantKinds: []string{KindProjectLiveness},
		},
		{
			name: "empty queue without completions stays quiet",
			input: Input{
				ProjectID: "detent",
			},
		},
		{
			name: "recently populated queue stays quiet",
			input: Input{
				ProjectID:    "detent",
				Dispatchable: []Item{{ID: "1", EnteredAt: now.Add(-time.Hour)}},
				Completions:  []Completion{{At: now.Add(-48 * time.Hour)}},
			},
		},
		{
			name: "merge queue without success warns",
			input: Input{
				ProjectID:  "detent",
				MergeQueue: []Item{{ID: "2", EnteredAt: now.Add(-7 * time.Hour)}},
			},
			wantKinds: []string{KindMergeLiveness},
		},
		{
			name: "repeated identical decisions warn",
			input: Input{
				ProjectID: "detent",
				Decisions: []Decision{
					{IssueID: "3", Reason: "merge_slot_revoked", At: now.Add(-3 * time.Hour)},
					{IssueID: "3", Reason: "merge_slot_revoked", At: now.Add(-2 * time.Hour)},
					{IssueID: "3", Reason: "merge_slot_revoked", At: now.Add(-time.Hour)},
					{IssueID: "3", Reason: "other", At: now.Add(-time.Hour)},
				},
			},
			wantKinds: []string{KindRepeatedDecision},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(cfg, tt.input, now)
			if len(got) != len(tt.wantKinds) {
				t.Fatalf("Evaluate() warnings = %#v, want kinds %v", got, tt.wantKinds)
			}
			for index, kind := range tt.wantKinds {
				if got[index].Kind != kind {
					t.Fatalf("Evaluate()[%d].Kind = %q, want %q", index, got[index].Kind, kind)
				}
			}
			if len(got) > 0 && got[0].WaitingOnHuman != tt.wantHuman {
				t.Fatalf("Evaluate()[0].WaitingOnHuman = %t, want %t", got[0].WaitingOnHuman, tt.wantHuman)
			}
		})
	}
}

func TestNewNotifierDeliversWebhook(t *testing.T) {
	t.Parallel()
	received := make(chan Notification, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer example" {
			t.Errorf("Authorization = %q, want Bearer example", got)
		}
		var payload Notification
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		received <- payload
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	notifier, err := NewNotifier(DeliveryConfig{
		WebhookURL: server.URL,
		Headers:    map[string]string{"Authorization": "Bearer example"},
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("NewNotifier() error = %v", err)
	}
	warning := Warning{
		ID:               "warning-1",
		Kind:             KindLaneAging,
		AgeSeconds:       3600,
		ThresholdSeconds: 1800,
	}
	if err := notifier.Notify(t.Context(), warning); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	payload := <-received
	if payload.Schema != 1 || payload.Event != "detent.staleness.warning" || payload.Warning.ID != warning.ID {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Warning.AgeSeconds != 3600 || payload.Warning.ThresholdSeconds != 1800 {
		t.Fatalf("payload warning durations = %#v", payload.Warning)
	}
}

func TestNewNotifierRejectsMissingURL(t *testing.T) {
	t.Parallel()
	if _, err := NewNotifier(DeliveryConfig{}); err == nil {
		t.Fatal("NewNotifier() error = nil, want missing URL error")
	}
}
