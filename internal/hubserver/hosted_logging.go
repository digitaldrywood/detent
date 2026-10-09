package hubserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hostedLogError struct{ err error }

func (e hostedLogError) Error() string { return hostedLogText(e.err.Error()) }
func (e hostedLogError) Unwrap() error { return e.err }

type hostedLogHandler struct {
	output    slog.Handler
	dropAttrs bool
}

var hostedErrorClass = regexp.MustCompile(`^[A-Za-z0-9_.*]{1,80}$`)

var hostedRouteTemplate = regexp.MustCompile(`^/[A-Za-z0-9_/:.*-]{0,200}$`)

var hostedTimingCounters = map[string]bool{"status": true, "duration_ms": true, "writer_hold_ms": true, "writer_txs": true,
	"waits": true, "wait_ms": true, "in_use": true, "interval_s": true}

func (h hostedLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.output.Enabled(ctx, level)
}

func (h hostedLogHandler) Handle(ctx context.Context, record slog.Record) error {
	redacted := slog.NewRecord(record.Time, record.Level, hostedLogText(record.Message), record.PC)
	if !h.dropAttrs {
		record.Attrs(func(attr slog.Attr) bool {
			if attr, ok := hostedLogAttr(attr); ok {
				redacted.AddAttrs(attr)
			}
			return true
		})
	}
	return h.output.Handle(ctx, redacted)
}

func hostedContentKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
	switch key {
	case "title", "issuetitle", "body", "issuebody", "description", "comment", "comments", "commentbody", "commenttext", "prompt", "prompts", "systemprompt", "userprompt", "conversation", "conversationtext", "message", "messages", "text", "content", "input", "output", "arguments", "requestbody", "responsebody", "email", "supportreason":
		return true
	}
	return strings.Contains(key, "secret") || strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "apikey") || key == "authorization" || key == "cookie" || key == "code" || key == "verifier"
}

func hostedTimingAttr(attr slog.Attr) bool {
	switch attr.Key {
	case "method":
		return attr.Value.Kind() == slog.KindString && hostedErrorClass.MatchString(attr.Value.String())
	case "route":
		return attr.Value.Kind() == slog.KindString && hostedRouteTemplate.MatchString(attr.Value.String())
	}
	kind := attr.Value.Kind()
	return hostedTimingCounters[attr.Key] && (kind == slog.KindInt64 || kind == slog.KindUint64)
}

func hostedLogText(value string) string {
	return healthEmailPrivateText.ReplaceAllStringFunc(strings.Join(strings.Fields(value), " "), func(match string) string {
		if healthEmailSubjectID.MatchString(match) || uuid.Validate(match) == nil {
			return match
		}
		return "[redacted]"
	})
}

func hostedLogAttr(attr slog.Attr) (slog.Attr, bool) {
	if hostedContentKey(attr.Key) {
		return slog.Attr{}, false
	}
	attr.Value = attr.Value.Resolve()
	if (attr.Key == "method" || attr.Key == "route" || hostedTimingCounters[attr.Key]) && !hostedTimingAttr(attr) {
		return slog.Attr{}, false
	}
	if attr.Value.Kind() == slog.KindGroup {
		attrs := make([]slog.Attr, 0, len(attr.Value.Group()))
		for _, child := range attr.Value.Group() {
			if next, ok := hostedLogAttr(child); ok {
				attrs = append(attrs, next)
			}
		}
		attr.Value = slog.GroupValue(attrs...)
		return attr, len(attrs) > 0
	}
	if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.AnyValue(hostedLogError{err: err})
			return attr, true
		}
		raw, err := json.Marshal(attr.Value.Any())
		if err != nil {
			return slog.Attr{}, false
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return slog.Attr{}, false
		}
		attr.Value = slog.AnyValue(hostedLogValue(value))
	}
	if attr.Value.Kind() == slog.KindString {
		value := attr.Value.String()
		switch attr.Key {
		case "correlation_id":
			if uuid.Validate(value) != nil {
				return slog.Attr{}, false
			}
		case "error_class":
			if !hostedErrorClass.MatchString(value) {
				return slog.Attr{}, false
			}
		case "tool":
			if _, known := operatortool.Lookup(value); !known {
				return slog.Attr{}, false
			}
		}
		attr.Value = slog.StringValue(hostedLogText(value))
	}
	return attr, true
}

func hostedLogValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			if !hostedContentKey(key) {
				result[key] = hostedLogValue(child)
			}
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = hostedLogValue(child)
		}
		return result
	case string:
		return hostedLogText(value)
	default:
		return value
	}
}

func (h hostedLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.dropAttrs {
		return h
	}
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if next, ok := hostedLogAttr(attr); ok {
			filtered = append(filtered, next)
		}
	}
	h.output = h.output.WithAttrs(filtered)
	return h
}
func (h hostedLogHandler) WithGroup(name string) slog.Handler {
	if hostedContentKey(name) {
		h.dropAttrs = true
		return h
	}
	h.output = h.output.WithGroup(name)
	return h
}
