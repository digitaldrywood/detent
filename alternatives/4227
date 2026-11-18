package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lmittmann/tint"
)

type SourceSetting struct {
	Value bool
	Set   bool
}

func SourceFromEnv(lookup func(string) string) SourceSetting {
	for _, key := range []string{"LOG_ADD_SOURCE", "DETENT_LOG_ADD_SOURCE"} {
		if value := strings.TrimSpace(lookup(key)); value != "" {
			return SourceSetting{Value: strings.Contains("|1|t|true|y|yes|on|", "|"+strings.ToLower(value)+"|"), Set: true}
		}
	}
	return SourceSetting{}
}

type logWriter struct {
	mu     sync.Mutex
	output io.Writer
}

func (w *logWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.Write(data)
}

type binding struct {
	groups []string
	attrs  []slog.Attr
}

type handler struct {
	withSource    slog.Handler
	withoutSource slog.Handler
	source        SourceSetting
	bindings      []binding
	groups        []string
}

func NewHandler(w io.Writer, level slog.Leveler, text bool, source SourceSetting) slog.Handler {
	if w == nil {
		w = io.Discard
	}
	w = &logWriter{output: w}
	makeHandler := func(addSource bool) slog.Handler {
		if text {
			return tint.NewHandler(w, &tint.Options{Level: level, TimeFormat: time.Kitchen, AddSource: addSource, ReplaceAttr: textReplaceAttr})
		}
		return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, AddSource: addSource, ReplaceAttr: replaceAttr})
	}
	return handler{withSource: makeHandler(true), withoutSource: makeHandler(false), source: source}
}

func (h handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.withoutSource.Enabled(ctx, level)
}

func (h handler) Handle(ctx context.Context, record slog.Record) error {
	var failure error
	var failureText string
	var suppliedClass slog.Attr
	var normalize func(slog.Attr) slog.Attr
	normalize = func(attr slog.Attr) slog.Attr {
		attr.Value = attr.Value.Resolve()
		if attr.Key == slog.SourceKey {
			attr.Key = "event_source"
		}
		if attr.Key == "error_class" {
			suppliedClass = attr
			return slog.Attr{}
		}
		_, typedError := attr.Value.Any().(error)
		if attr.Key == "err" || attr.Key == "error" || typedError {
			if err, ok := attr.Value.Any().(error); ok {
				failure = err
				failureText = err.Error()
			} else {
				failureText = attr.Value.String()
			}
			return slog.Attr{}
		}
		if attr.Value.Kind() == slog.KindGroup {
			attrs := make([]slog.Attr, 0, len(attr.Value.Group()))
			for _, child := range attr.Value.Group() {
				if next := normalize(child); !next.Equal(slog.Attr{}) {
					attrs = append(attrs, next)
				}
			}
			attr.Value = slog.GroupValue(attrs...)
		}
		return attr
	}
	normalized := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	add := func(groups []string, attrs []slog.Attr) {
		filtered := make([]slog.Attr, 0, len(attrs))
		for _, attr := range attrs {
			if attr = normalize(attr); !attr.Equal(slog.Attr{}) {
				filtered = append(filtered, attr)
			}
		}
		for i := len(groups) - 1; i >= 0; i-- {
			filtered = []slog.Attr{{Key: groups[i], Value: slog.GroupValue(filtered...)}}
		}
		normalized.AddAttrs(filtered...)
	}
	for _, b := range h.bindings {
		add(b.groups, b.attrs)
	}
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool { attrs = append(attrs, attr); return true })
	add(h.groups, attrs)
	if failureText != "" || failure != nil {
		normalized.AddAttrs(slog.Any("error", errors.New(strings.NewReplacer("\r", " ", "\n", " ").Replace(failureText))))
		if failure != nil {
			normalized.AddAttrs(slog.String("error_class", ErrorClass(failure)))
		} else if !suppliedClass.Equal(slog.Attr{}) {
			normalized.AddAttrs(suppliedClass)
		} else {
			normalized.AddAttrs(slog.String("error_class", "*errors.errorString"))
		}
	}
	if failure == nil && failureText == "" && !suppliedClass.Equal(slog.Attr{}) {
		normalized.AddAttrs(suppliedClass)
	}
	source := record.Level >= slog.LevelWarn || record.Level <= slog.LevelDebug
	if h.source.Set {
		source = h.source.Value
	}
	if source {
		return h.withSource.Handle(ctx, normalized)
	}
	return h.withoutSource.Handle(ctx, normalized)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.bindings = append(append([]binding(nil), h.bindings...), binding{groups: append([]string(nil), h.groups...), attrs: append([]slog.Attr(nil), attrs...)})
	return h
}
func (h handler) WithGroup(name string) slog.Handler {
	if name != "" {
		h.groups = append(append([]string(nil), h.groups...), name)
	}
	return h
}
func textReplaceAttr(groups []string, attr slog.Attr) slog.Attr {
	attr = replaceAttr(groups, attr)
	if err, ok := attr.Value.Any().(error); ok {
		next := tint.Err(err)
		next.Key = attr.Key
		return next
	}
	return attr
}
func replaceAttr(_ []string, attr slog.Attr) slog.Attr {
	if attr.Key == slog.SourceKey {
		if source, ok := attr.Value.Any().(*slog.Source); ok && source != nil {
			copied := *source
			copied.File = CleanSourcePath(copied.File)
			return slog.Any(slog.SourceKey, &copied)
		}
	}
	return attr
}

func ErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		return "sqlite_" + strconv.Itoa(coded.Code())
	}
	for {
		next := errors.Unwrap(err)
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			if causes := joined.Unwrap(); len(causes) > 0 {
				next = causes[len(causes)-1]
			}
		}
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}

func CleanSourcePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return path
	}
	if rel, ok := relativeSourcePath(path, mustGetwdForLogSource()); ok {
		return rel
	}
	if rel, ok := moduleRelativeSourcePath(path); ok {
		return rel
	}
	return filepath.ToSlash(path)
}

func relativeSourcePath(path string, base string) (string, bool) {
	if strings.TrimSpace(base) == "" {
		return "", false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func moduleRelativeSourcePath(path string) (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok || strings.TrimSpace(info.Main.Path) == "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(info.Main.Path, "/"), "/")
	if len(parts) == 0 {
		return "", false
	}
	module := parts[len(parts)-1]
	normalized := filepath.ToSlash(path)
	marker := "/" + module + "/"
	index := strings.LastIndex(normalized, marker)
	if index == -1 {
		return "", false
	}
	return normalized[index+len(marker):], true
}

func mustGetwdForLogSource() string {
	wd, err := os.Getwd()
	if err != nil {
		return string(filepath.Separator)
	}
	return wd
}
