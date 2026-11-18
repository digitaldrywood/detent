package telemetry

import (
	"context"
	"log/slog"
	"strings"
)

const (
	LifecycleClassKey    = "lifecycle_class"
	LifecycleEventKey    = "event"
	ProjectIDKey         = "project_id"
	IssueIDKey           = "issue_id"
	IssueIdentifierKey   = "issue_identifier"
	WorkAttemptIDKey     = "work_attempt_id"
	DetentSessionIDKey   = "detent_session_id"
	ProviderSessionIDKey = "provider_session_id"
)

type LifecycleClass string

const (
	LifecycleDispatch        LifecycleClass = "dispatch"
	LifecycleWorkAttempt     LifecycleClass = "work_attempt"
	LifecycleWorkspace       LifecycleClass = "workspace"
	LifecycleDetentSession   LifecycleClass = "detent_session"
	LifecycleProviderSession LifecycleClass = "provider_session"
	LifecycleSafetyControl   LifecycleClass = "safety_control"
	LifecycleAdmission       LifecycleClass = "admission"
)

var lifecycleClasses = [...]LifecycleClass{
	LifecycleDispatch,
	LifecycleWorkAttempt,
	LifecycleWorkspace,
	LifecycleDetentSession,
	LifecycleProviderSession,
	LifecycleSafetyControl,
	LifecycleAdmission,
}

type LifecycleCorrelation struct {
	ProjectID         string
	IssueID           string
	IssueIdentifier   string
	WorkAttemptID     int64
	DetentSessionID   int64
	ProviderSessionID string
}

func LifecycleClasses() []LifecycleClass {
	classes := make([]LifecycleClass, len(lifecycleClasses))
	copy(classes, lifecycleClasses[:])
	return classes
}

func LogLifecycle(logger *slog.Logger, level slog.Level, class LifecycleClass, event string, correlation LifecycleCorrelation, attrs ...any) {
	LogLifecycleMessage(logger, level, class, event, event, correlation, attrs...)
}

func LogLifecycleContext(ctx context.Context, logger *slog.Logger, level slog.Level, class LifecycleClass, event string, correlation LifecycleCorrelation, attrs ...any) {
	LogLifecycleMessageContext(ctx, logger, level, class, event, event, correlation, attrs...)
}

func LogLifecycleMessage(logger *slog.Logger, level slog.Level, class LifecycleClass, event string, message string, correlation LifecycleCorrelation, attrs ...any) {
	if logger == nil {
		return
	}
	all := lifecycleLogAttrs(class, event, correlation, attrs)
	message = strings.TrimSpace(message)
	switch {
	case level >= slog.LevelError:
		logger.Error(message, all...)
	case level >= slog.LevelWarn:
		logger.Warn(message, all...)
	case level >= slog.LevelInfo:
		logger.Info(message, all...)
	default:
		logger.Debug(message, all...)
	}
}

func LogLifecycleMessageContext(ctx context.Context, logger *slog.Logger, level slog.Level, class LifecycleClass, event string, message string, correlation LifecycleCorrelation, attrs ...any) {
	if logger == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	logger.Log(ctx, level, strings.TrimSpace(message), lifecycleLogAttrs(class, event, correlation, attrs)...)
}

func lifecycleLogAttrs(class LifecycleClass, event string, correlation LifecycleCorrelation, attrs []any) []any {
	all := []any{
		LifecycleClassKey, string(class),
		LifecycleEventKey, strings.TrimSpace(event),
	}
	all = appendLifecycleString(all, ProjectIDKey, correlation.ProjectID)
	all = appendLifecycleString(all, IssueIDKey, correlation.IssueID)
	all = appendLifecycleString(all, IssueIdentifierKey, correlation.IssueIdentifier)
	if correlation.WorkAttemptID > 0 {
		all = append(all, WorkAttemptIDKey, correlation.WorkAttemptID)
	}
	if correlation.DetentSessionID > 0 {
		all = append(all, DetentSessionIDKey, correlation.DetentSessionID)
	}
	all = appendLifecycleString(all, ProviderSessionIDKey, correlation.ProviderSessionID)
	all = appendLifecycleAttrs(all, attrs)
	return all
}

func appendLifecycleString(attrs []any, key string, value string) []any {
	value = strings.TrimSpace(value)
	if value == "" {
		return attrs
	}
	return append(attrs, key, value)
}

func appendLifecycleAttrs(destination []any, attrs []any) []any {
	for index := 0; index < len(attrs); {
		if attr, ok := attrs[index].(slog.Attr); ok {
			if !lifecycleReservedKey(attr.Key) {
				destination = append(destination, attr)
			}
			index++
			continue
		}
		if index+1 >= len(attrs) {
			destination = append(destination, attrs[index])
			break
		}
		key, ok := attrs[index].(string)
		if !ok || !lifecycleReservedKey(key) {
			destination = append(destination, attrs[index], attrs[index+1])
		}
		index += 2
	}
	return destination
}

func lifecycleReservedKey(key string) bool {
	switch key {
	case LifecycleClassKey, LifecycleEventKey, ProjectIDKey, IssueIDKey, IssueIdentifierKey, WorkAttemptIDKey, DetentSessionIDKey, ProviderSessionIDKey:
		return true
	default:
		return false
	}
}
