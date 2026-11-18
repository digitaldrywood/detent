package staleness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/notify"
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
	webhook, err := notify.NewWebhook(notify.WebhookConfig{
		URL:       cfg.WebhookURL,
		Headers:   cfg.Headers,
		Timeout:   cfg.Timeout,
		UserAgent: "detent-staleness-watchdog",
	})
	if err != nil {
		return nil, err
	}
	return &webhookNotifier{webhook: webhook}, nil
}

type webhookNotifier struct {
	webhook *notify.Webhook
}

func (n *webhookNotifier) Notify(ctx context.Context, warning Warning) error {
	if n == nil || n.webhook == nil {
		return errors.New("staleness webhook notifier is not configured")
	}
	if err := n.webhook.Send(ctx, Notification{
		Schema:  1,
		Event:   "detent.staleness.warning",
		Warning: warning,
	}); err != nil {
		return fmt.Errorf("deliver staleness webhook: %w", err)
	}
	return nil
}
