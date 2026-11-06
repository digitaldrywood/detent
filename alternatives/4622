package templates

import (
	"strings"
	"time"
)

type LocalTimeStyle string

const (
	LocalTimeOnly        LocalTimeStyle = "time"
	LocalTimeWithSeconds LocalTimeStyle = "time-seconds"
	LocalDateOnly        LocalTimeStyle = "date"
	LocalDateTime        LocalTimeStyle = "date-time"
	LocalDateTimeSeconds LocalTimeStyle = "date-time-seconds"
	LocalDateTimeZone    LocalTimeStyle = "date-time-zone"
)

type LocalTimeOptions struct {
	Style    LocalTimeStyle
	Relative bool
	Fallback string
}

func localTimeStyle(options LocalTimeOptions) string {
	if options.Style == "" {
		return string(LocalTimeOnly)
	}
	return string(options.Style)
}

func localTimeFallback(options LocalTimeOptions) string {
	if options.Fallback != "" {
		return options.Fallback
	}
	return "…"
}

func localTimeISOString(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func localTimeToken(value time.Time, style LocalTimeStyle) string {
	if value.IsZero() {
		return ""
	}
	if style == "" {
		style = LocalTimeOnly
	}
	return "{{detent-time:" + strings.TrimSpace(string(style)) + ":" + localTimeISOString(value) + "}}"
}
