package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookSend(t *testing.T) {
	t.Parallel()
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer example" {
			t.Errorf("Authorization = %q, want Bearer example", got)
		}
		if got := request.Header.Get("User-Agent"); got != "detent-test" {
			t.Errorf("User-Agent = %q, want detent-test", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		received <- payload
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	webhook, err := NewWebhook(WebhookConfig{
		URL:       server.URL,
		Headers:   map[string]string{"Authorization": "Bearer example"},
		Timeout:   time.Second,
		UserAgent: "detent-test",
	})
	if err != nil {
		t.Fatalf("NewWebhook() error = %v", err)
	}
	if err := webhook.Send(t.Context(), map[string]string{"event": "test"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := (<-received)["event"]; got != "test" {
		t.Fatalf("event = %#v, want test", got)
	}
}

func TestWebhookErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		url     string
		want    string
		notWant string
	}{
		{name: "missing URL", want: "webhook URL is required"},
		{name: "HTTP failure", url: "server", want: "HTTP 503 Service Unavailable", notWant: "receiver-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url := tt.url
			if url == "server" {
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					http.Error(writer, "receiver-secret", http.StatusServiceUnavailable)
				}))
				t.Cleanup(server.Close)
				url = server.URL
			}
			webhook, err := NewWebhook(WebhookConfig{URL: url})
			if err != nil {
				if err.Error() != tt.want {
					t.Fatalf("NewWebhook() error = %q, want %q", err, tt.want)
				}
				return
			}
			if err := webhook.Send(t.Context(), struct{}{}); err == nil || !strings.Contains(err.Error(), tt.want) || (tt.notWant != "" && strings.Contains(err.Error(), tt.notWant)) {
				t.Fatalf("Send() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
