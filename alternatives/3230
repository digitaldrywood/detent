package telemetry

import (
	"context"
	"log/slog"
	"slices"
	"testing"
)

func TestLogLifecycleCorrelationContract(t *testing.T) {
	t.Parallel()

	wantClasses := []LifecycleClass{
		LifecycleDispatch,
		LifecycleWorkAttempt,
		LifecycleWorkspace,
		LifecycleDetentSession,
		LifecycleProviderSession,
		LifecycleSafetyControl,
		LifecycleAdmission,
	}
	if got := LifecycleClasses(); !slices.Equal(got, wantClasses) {
		t.Fatalf("LifecycleClasses() = %v, want %v", got, wantClasses)
	}

	for _, class := range wantClasses {
		t.Run(string(class), func(t *testing.T) {
			t.Parallel()

			handler := &capturedLifecycleHandler{}
			LogLifecycle(slog.New(handler), slog.LevelInfo, class, " lifecycle_event ", LifecycleCorrelation{
				ProjectID:         " project-1 ",
				IssueID:           " issue-2 ",
				IssueIdentifier:   " owner/repo#2 ",
				WorkAttemptID:     3,
				DetentSessionID:   4,
				ProviderSessionID: " provider-session-5 ",
			})

			record := handler.record(t)
			if record.message != "lifecycle_event" || record.level != slog.LevelInfo {
				t.Errorf("record message/level = %q/%s, want lifecycle_event/INFO", record.message, record.level)
			}
			want := map[string]any{
				LifecycleClassKey:    string(class),
				LifecycleEventKey:    "lifecycle_event",
				ProjectIDKey:         "project-1",
				IssueIDKey:           "issue-2",
				IssueIdentifierKey:   "owner/repo#2",
				WorkAttemptIDKey:     int64(3),
				DetentSessionIDKey:   int64(4),
				ProviderSessionIDKey: "provider-session-5",
			}
			for key, value := range want {
				if got := record.attrs[key]; got != value {
					t.Errorf("attribute %q = %#v, want %#v", key, got, value)
				}
			}
		})
	}
}

func TestLogLifecycleOmitsUnknownCorrelation(t *testing.T) {
	t.Parallel()

	handler := &capturedLifecycleHandler{}
	LogLifecycle(slog.New(handler), slog.LevelInfo, LifecycleWorkAttempt, "worker_attempt_started", LifecycleCorrelation{
		ProjectID:         " ",
		IssueID:           "",
		IssueIdentifier:   " ",
		WorkAttemptID:     0,
		DetentSessionID:   -1,
		ProviderSessionID: " ",
	})

	record := handler.record(t)
	for _, key := range []string{
		ProjectIDKey,
		IssueIDKey,
		IssueIdentifierKey,
		WorkAttemptIDKey,
		DetentSessionIDKey,
		ProviderSessionIDKey,
	} {
		if _, ok := record.attrs[key]; ok {
			t.Errorf("unknown correlation attribute %q was emitted", key)
		}
	}
}

func TestLogLifecycleProtectsCanonicalAttributes(t *testing.T) {
	t.Parallel()

	handler := &capturedLifecycleHandler{}
	LogLifecycle(slog.New(handler), slog.LevelInfo, LifecycleWorkAttempt, "worker_attempt_started", LifecycleCorrelation{
		ProjectID:     "project-1",
		IssueID:       "issue-2",
		WorkAttemptID: 3,
	},
		ProjectIDKey, "wrong-project",
		WorkAttemptIDKey, "wrong-type",
		slog.String(IssueIDKey, "wrong-issue"),
	)

	record := handler.record(t)
	if got := record.attrs[ProjectIDKey]; got != "project-1" {
		t.Errorf("%s = %#v, want project-1", ProjectIDKey, got)
	}
	if got := record.attrs[IssueIDKey]; got != "issue-2" {
		t.Errorf("%s = %#v, want issue-2", IssueIDKey, got)
	}
	if got := record.attrs[WorkAttemptIDKey]; got != int64(3) {
		t.Errorf("%s = %#v, want int64(3)", WorkAttemptIDKey, got)
	}
}

type capturedLifecycleRecord struct {
	message string
	level   slog.Level
	attrs   map[string]any
}

type capturedLifecycleHandler struct {
	records []capturedLifecycleRecord
}

func (h *capturedLifecycleHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *capturedLifecycleHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := map[string]any{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	h.records = append(h.records, capturedLifecycleRecord{
		message: record.Message,
		level:   record.Level,
		attrs:   attrs,
	})
	return nil
}

func (h *capturedLifecycleHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *capturedLifecycleHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *capturedLifecycleHandler) record(t *testing.T) capturedLifecycleRecord {
	t.Helper()
	if len(h.records) != 1 {
		t.Fatalf("captured %d records, want 1", len(h.records))
	}
	return h.records[0]
}
