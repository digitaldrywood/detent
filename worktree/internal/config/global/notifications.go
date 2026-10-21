package global

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultHealthNotificationDebounceSeconds = 300
	DefaultNotificationWebhookTimeoutMS      = 5000
)

type Notifications struct {
	Health HealthNotifications `yaml:"health,omitempty"`
}

type HealthNotifications struct {
	DebounceSeconds int                 `yaml:"debounce_seconds,omitempty"`
	Webhook         NotificationWebhook `yaml:"webhook,omitempty"`
}

type NotificationWebhook struct {
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	TimeoutMS int               `yaml:"timeout_ms,omitempty"`
}

func (n Notifications) IsZero() bool {
	return n.Health.IsZero()
}

func (h HealthNotifications) IsZero() bool {
	return h.DebounceSeconds == 0 && h.Webhook.IsZero()
}

func (w NotificationWebhook) IsZero() bool {
	return strings.TrimSpace(w.URL) == "" && len(w.Headers) == 0 && w.TimeoutMS == 0
}

func (h HealthNotifications) Debounce() time.Duration {
	if h.DebounceSeconds <= 0 {
		return time.Duration(DefaultHealthNotificationDebounceSeconds) * time.Second
	}
	return time.Duration(h.DebounceSeconds) * time.Second
}

func (w NotificationWebhook) Timeout() time.Duration {
	if w.TimeoutMS <= 0 {
		return time.Duration(DefaultNotificationWebhookTimeoutMS) * time.Millisecond
	}
	return time.Duration(w.TimeoutMS) * time.Millisecond
}

func (n Notifications) Validate() []string {
	var problems []string
	health := n.Health
	if health.DebounceSeconds < 0 {
		problems = append(problems, "notifications.health.debounce_seconds must be greater than 0")
	}
	webhook := health.Webhook
	webhook.URL = strings.TrimSpace(webhook.URL)
	if webhook.URL == "" {
		if len(webhook.Headers) > 0 || webhook.TimeoutMS != 0 {
			problems = append(problems, "notifications.health.webhook.url is required when webhook settings are configured")
		}
		return problems
	}
	parsed, err := url.Parse(webhook.URL)
	if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(parsed.Host) == "" {
		problems = append(problems, "notifications.health.webhook.url must be an absolute http or https URL")
	}
	if webhook.TimeoutMS < 0 {
		problems = append(problems, "notifications.health.webhook.timeout_ms must be greater than 0")
	}
	return problems
}

func notificationsRawErrors(value any) []string {
	if value == nil {
		return nil
	}
	attrs, ok := value.(map[string]any)
	if !ok {
		return []string{"notifications: must be a mapping"}
	}
	healthValue, configured := attrs["health"]
	if !configured || healthValue == nil {
		return nil
	}
	health, ok := healthValue.(map[string]any)
	if !ok {
		return []string{"notifications.health: must be a mapping"}
	}
	var problems []string
	if debounce, configured := health["debounce_seconds"]; configured && !positiveInteger(debounce) {
		problems = append(problems, "notifications.health.debounce_seconds: must be a positive integer")
	}
	problems = append(problems, notificationWebhookRawErrors(health["webhook"])...)
	return problems
}

func notificationWebhookRawErrors(value any) []string {
	if value == nil {
		return nil
	}
	attrs, ok := value.(map[string]any)
	if !ok {
		return []string{"notifications.health.webhook: must be a mapping"}
	}
	var problems []string
	problems = append(problems, optionalStringTypeError(attrs, "url")...)
	if timeout, configured := attrs["timeout_ms"]; configured && !positiveInteger(timeout) {
		problems = append(problems, "notifications.health.webhook.timeout_ms: must be a positive integer")
	}
	if headersValue, configured := attrs["headers"]; configured {
		headers, ok := headersValue.(map[string]any)
		if !ok {
			problems = append(problems, "notifications.health.webhook.headers: must be a mapping")
		} else {
			for name, value := range headers {
				if _, ok := value.(string); !ok {
					problems = append(problems, fmt.Sprintf("notifications.health.webhook.headers.%s: must be a string", name))
				}
			}
		}
	}
	return problems
}

func buildNotifications(value any) (Notifications, error) {
	if value == nil {
		return Notifications{}, nil
	}
	if _, err := mapValue(value, "notifications"); err != nil {
		return Notifications{}, err
	}
	var notifications Notifications
	if err := decodeYAMLValue(value, &notifications); err != nil {
		return Notifications{}, fmt.Errorf("notifications: %w", err)
	}
	notifications.Health.Webhook.URL = strings.TrimSpace(notifications.Health.Webhook.URL)
	if len(notifications.Health.Webhook.Headers) > 0 {
		headers := make(map[string]string, len(notifications.Health.Webhook.Headers))
		for name, value := range notifications.Health.Webhook.Headers {
			name = strings.TrimSpace(name)
			if name != "" {
				headers[name] = strings.TrimSpace(value)
			}
		}
		notifications.Health.Webhook.Headers = headers
	}
	return notifications, nil
}
