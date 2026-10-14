package efficiency

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ExporterConfig struct {
	Endpoint    string
	Headers     map[string]string
	ServiceName string
	Timeout     time.Duration
}

type LifecycleExporter interface {
	ExportLifecycle(context.Context, Receipt) error
}

type disabledExporter struct{}

func (disabledExporter) ExportLifecycle(context.Context, Receipt) error {
	return nil
}

type otlpHTTPExporter struct {
	endpoint    string
	headers     map[string]string
	serviceName string
	client      *http.Client
}

func NewLifecycleExporter(cfg ExporterConfig) (LifecycleExporter, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return disabledExporter{}, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("OTLP endpoint must be an absolute http or https URL")
	}
	if !strings.HasSuffix(parsed.Path, "/v1/traces") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/traces"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "detent"
	}
	return &otlpHTTPExporter{
		endpoint:    parsed.String(),
		headers:     cloneHeaders(cfg.Headers),
		serviceName: serviceName,
		client:      &http.Client{Timeout: timeout},
	}, nil
}

func (e *otlpHTTPExporter) ExportLifecycle(ctx context.Context, receipt Receipt) error {
	traceID, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("create OTLP trace id: %w", err)
	}
	spans, err := lifecycleSpans(traceID, receipt)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{otlpAttribute("service.name", e.serviceName)}},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "github.com/digitaldrywood/detent/internal/efficiency"},
				"spans": spans,
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode OTLP lifecycle: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create OTLP lifecycle request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range e.headers {
		req.Header.Set(name, value)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("export OTLP lifecycle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("export OTLP lifecycle: collector returned %s", resp.Status)
	}
	return nil
}

func lifecycleSpans(traceID string, receipt Receipt) ([]any, error) {
	start := receipt.FirstDispatchedAt
	if start.IsZero() {
		start = receipt.CompletedAt.Add(-time.Duration(receipt.WallSeconds) * time.Second)
	}
	end := receipt.CompletedAt
	if end.Before(start) {
		end = start
	}
	durations := []time.Duration{
		time.Second,
		time.Duration(receipt.WorkingSeconds) * time.Second,
		time.Duration(receipt.GateWaitSeconds+receipt.ParkedSeconds) * time.Second,
		time.Duration(receipt.MergeTrainSeconds) * time.Second,
	}
	names := []string{"detent.dispatch", "detent.session", "detent.gate", "detent.merge"}
	spans := make([]any, 0, len(names))
	parentID := ""
	cursor := start
	for i, name := range names {
		spanID, err := randomHex(8)
		if err != nil {
			return nil, fmt.Errorf("create OTLP span id: %w", err)
		}
		spanEnd := cursor.Add(durations[i])
		if i == len(names)-1 || spanEnd.After(end) {
			spanEnd = end
		}
		if spanEnd.Before(cursor) {
			spanEnd = cursor
		}
		span := map[string]any{
			"traceId":           traceID,
			"spanId":            spanID,
			"name":              name,
			"kind":              1,
			"startTimeUnixNano": strconv.FormatInt(cursor.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(spanEnd.UnixNano(), 10),
			"attributes": []any{
				otlpAttribute("detent.project.id", receipt.ProjectID),
				otlpAttribute("detent.issue.id", receipt.IssueID),
				otlpAttribute("detent.issue.identifier", receipt.Identifier),
				otlpAttribute("detent.tokens.total", receipt.TotalTokens),
				otlpAttribute("detent.cache.share", receipt.CacheShare()),
				otlpAttribute("detent.cost.usd", receipt.EstimatedCostUSD),
			},
			"status": map[string]any{"code": 1},
		}
		if parentID != "" {
			span["parentSpanId"] = parentID
		}
		spans = append(spans, span)
		parentID = spanID
		cursor = spanEnd
	}
	return spans, nil
}

func otlpAttribute(key string, value any) map[string]any {
	typed := map[string]any{}
	switch value := value.(type) {
	case string:
		typed["stringValue"] = value
	case int64:
		typed["intValue"] = strconv.FormatInt(value, 10)
	case float64:
		typed["doubleValue"] = value
	case bool:
		typed["boolValue"] = value
	}
	return map[string]any{"key": key, "value": typed}
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func cloneHeaders(headers map[string]string) map[string]string {
	cloned := make(map[string]string, len(headers))
	for name, value := range headers {
		if strings.TrimSpace(name) != "" {
			cloned[name] = value
		}
	}
	return cloned
}
