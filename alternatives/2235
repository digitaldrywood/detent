package intake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type webhookFactory struct{}

type jsonAdapter struct {
	summaryAliases     []string
	detailsAliases     []string
	fingerprintAliases []string
}

func DefaultWebhookFactory() WebhookFactory {
	return webhookFactory{}
}

func (webhookFactory) New(kind string) (WebhookAdapter, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindWebhook:
		return jsonAdapter{
			summaryAliases:     []string{"summary", "title", "message"},
			detailsAliases:     []string{"details", "description", "body", "message"},
			fingerprintAliases: []string{"fingerprint", "dedupe_key", "event_id", "id"},
		}, nil
	case KindSentry:
		return jsonAdapter{
			summaryAliases:     []string{"data.event.title", "data.event.message", "message", "action"},
			detailsAliases:     []string{"data.event.message", "data.event.culprit", "message"},
			fingerprintAliases: []string{"data.event.fingerprint", "data.event.event_id", "event_id", "id"},
		}, nil
	case KindDatadog:
		return jsonAdapter{
			summaryAliases:     []string{"title", "event_title", "alert_title", "message"},
			detailsAliases:     []string{"body", "event_msg", "message"},
			fingerprintAliases: []string{"alert_id", "event_id", "aggregation_key", "id"},
		}, nil
	case KindSlack:
		return jsonAdapter{
			summaryAliases:     []string{"event.text", "text", "event.type", "type"},
			detailsAliases:     []string{"event.text", "text"},
			fingerprintAliases: []string{"event_id", "event.client_msg_id", "event.ts", "trigger_id"},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownAdapter, kind)
	}
}

func (a jsonAdapter) Decode(raw []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrInvalidPayload, err)
	}
	if len(payload) == 0 {
		return Event{}, fmt.Errorf("%w: object must not be empty", ErrInvalidPayload)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Event{}, fmt.Errorf("%w: payload must contain one JSON object", ErrInvalidPayload)
	}

	fields := map[string]string{}
	flattenJSON("", payload, fields)
	summary := firstField(fields, a.summaryAliases)
	if summary == "" {
		return Event{}, fmt.Errorf("%w: summary is required", ErrInvalidPayload)
	}
	details := firstField(fields, a.detailsAliases)
	if details == "" {
		details = summary
	}
	fingerprint := firstField(fields, a.fingerprintAliases)
	if fingerprint != "" {
		fields["fingerprint"] = fingerprint
	}
	if fields["level"] == "" {
		fields["level"] = firstField(fields, []string{"data.event.level", "event.level", "alert_type", "priority"})
	}
	return Event{
		Summary:     summary,
		Details:     details,
		Fingerprint: fingerprint,
		Fields:      fields,
	}, nil
}

func flattenJSON(prefix string, value any, fields map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			flattenJSON(path, nested, fields)
		}
	case nil:
	case string:
		fields[prefix] = strings.TrimSpace(typed)
	case json.Number:
		fields[prefix] = typed.String()
	case bool:
		fields[prefix] = strconv.FormatBool(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err == nil {
			fields[prefix] = string(encoded)
		}
	}
}

func firstField(fields map[string]string, aliases []string) string {
	for _, alias := range aliases {
		if value := strings.TrimSpace(fields[alias]); value != "" {
			return value
		}
	}
	return ""
}
