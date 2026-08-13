package notify

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

const defaultWebhookTimeout = 5 * time.Second

type WebhookConfig struct {
	URL       string
	Headers   map[string]string
	Timeout   time.Duration
	UserAgent string
}

type Sender interface {
	Send(context.Context, any) error
}

type Webhook struct {
	url       string
	headers   map[string]string
	userAgent string
	client    *http.Client
}

func NewWebhook(cfg WebhookConfig) (*Webhook, error) {
	cfg.URL = strings.TrimSpace(cfg.URL)
	if cfg.URL == "" {
		return nil, errors.New("webhook URL is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultWebhookTimeout
	}
	return &Webhook{
		url:       cfg.URL,
		headers:   cloneHeaders(cfg.Headers),
		userAgent: strings.TrimSpace(cfg.UserAgent),
		client:    &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (w *Webhook) Send(ctx context.Context, payload any) error {
	if w == nil || w.client == nil {
		return errors.New("webhook is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if w.userAgent != "" {
		request.Header.Set("User-Agent", w.userAgent)
	}
	for name, value := range w.headers {
		request.Header.Set(name, value)
	}
	response, err := w.client.Do(request)
	if err != nil {
		return fmt.Errorf("deliver webhook: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
			return fmt.Errorf("drain webhook response: %w", err)
		}
		return nil
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return fmt.Errorf("read webhook response: %w", err)
	}
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("deliver webhook: HTTP %d: %s", response.StatusCode, detail)
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
