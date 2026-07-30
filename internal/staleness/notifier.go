package staleness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type DeliveryConfig struct {
	WebhookURL string
	Headers    map[string]string
	Timeout    time.Duration
}

type Notification struct {
	Schema  int     `json:"schema"`
	Event   string  `json:"event"`
	Warning Warning `json:"warning"`
}

type Notifier interface {
	Notify(context.Context, Warning) error
}

func NewNotifier(cfg DeliveryConfig) (Notifier, error) {
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	if cfg.WebhookURL == "" {
		return nil, errors.New("staleness webhook URL is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &webhookNotifier{
		url:     cfg.WebhookURL,
		headers: cloneHeaders(cfg.Headers),
		client:  &http.Client{Timeout: cfg.Timeout},
	}, nil
}

type webhookNotifier struct {
	url     string
	headers map[string]string
	client  *http.Client
}

func (n *webhookNotifier) Notify(ctx context.Context, warning Warning) error {
	if n == nil || n.client == nil {
		return errors.New("staleness webhook notifier is not configured")
	}
	payload, err := json.Marshal(Notification{
		Schema:  1,
		Event:   "detent.staleness.warning",
		Warning: warning,
	})
	if err != nil {
		return fmt.Errorf("marshal staleness webhook: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build staleness webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "detent-staleness-watchdog")
	for name, value := range n.headers {
		request.Header.Set(name, value)
	}
	response, err := n.client.Do(request)
	if err != nil {
		return fmt.Errorf("deliver staleness webhook: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
			return fmt.Errorf("drain staleness webhook response: %w", err)
		}
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return fmt.Errorf("read staleness webhook response: %w", err)
	}
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("deliver staleness webhook: HTTP %d: %s", response.StatusCode, detail)
}

func cloneHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(headers))
	for name, value := range headers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cloned[name] = strings.TrimSpace(value)
	}
	return cloned
}
