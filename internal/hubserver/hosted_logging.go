package hubserver

import (
	"context"
	"log/slog"
	"regexp"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hostedLogHandler struct {
	output slog.Handler
}

var hostedErrorClass = regexp.MustCompile(`^[A-Za-z0-9_.*]{1,80}$`)

func (h hostedLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.output.Enabled(ctx, level)
}

func (h hostedLogHandler) Handle(ctx context.Context, record slog.Record) error {
	redacted := slog.NewRecord(record.Time, record.Level, "hosted service event", 0)
	record.Attrs(func(attr slog.Attr) bool {
		if hostedDiagnosticAttr(attr) {
			redacted.AddAttrs(attr)
		}
		return true
	})
	return h.output.Handle(ctx, redacted)
}

func hostedDiagnosticAttr(attr slog.Attr) bool {
	if attr.Value.Kind() != slog.KindString {
		return false
	}
	value := attr.Value.String()
	switch attr.Key {
	case "correlation_id":
		return uuid.Validate(value) == nil
	case "error_class":
		return hostedErrorClass.MatchString(value)
	case "tool":
		_, known := operatortool.Lookup(value)
		return known
	}
	return false
}

func (h hostedLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h hostedLogHandler) WithGroup(string) slog.Handler {
	return h
}
