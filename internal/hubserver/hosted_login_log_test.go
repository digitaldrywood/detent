package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/logging"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hostedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *hostedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *hostedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestHostedLoginLogsStayRedactedOutsideDenials(t *testing.T) {
	t.Parallel()
	var output hostedLogBuffer
	cfg := hostedLoginConfig(t, newHostedLoginProvider(), true)
	cfg.Logger = slog.New(logging.NewHandler(&output, slog.LevelInfo, false, logging.SourceSetting{}))
	s := openTestService(t, cfg)
	s.config.Logger.Warn("hosted diagnostic", "title", "tenant content sentinel", "secret", "attribute-sentinel", "at", time.Now())
	logged := output.String()
	if strings.Contains(logged, "tenant content sentinel") || strings.Contains(logged, "attribute-sentinel") || !strings.Contains(logged, "hosted diagnostic") || !strings.Contains(logged, `"source":`) {
		t.Fatalf("hosted service log was not redacted: %s", logged)
	}

	var timing hostedLogBuffer
	logger := slog.New(hostedLogHandler{output: slog.NewJSONHandler(&timing, nil)})
	logger.Info("hub request timing", "route", "/example/private-route-sentinel", "owner", "secret=private-owner-sentinel", "title", "private-title-sentinel", "pool_full", "private-pool-sentinel")
	logger.Info("conversation.receipt", "text", "private-conversation-sentinel", "status", "private-status-sentinel")
	logger.Info("conversation.restart_normalized", "message", "private-message-sentinel", "token", 789123, "code", 456789)
	for _, private := range []string{"sentinel", "789123", "456789"} {
		if strings.Contains(timing.String(), private) {
			t.Fatal("timing or conversation logs contain private content")
		}
	}

	failure := fmt.Errorf("get change: load version: %w", &json.UnmarshalTypeError{Value: "array", Type: reflect.TypeFor[string]()})
	for _, tt := range []struct {
		name  string
		attrs []any
		want  map[string]any
	}{
		{"diagnostic fields kept", []any{"tool", operatortool.GetChange, "correlation_id", "6f1c1c55-6c0d-4b8e-9a43-0d6c2b7a9e10", "err", failure, "organization_id", "org_test", "project_id", "prj_test", "issue_id", "wi_test", "duration", 3}, map[string]any{"tool": operatortool.GetChange, "correlation_id": "6f1c1c55-6c0d-4b8e-9a43-0d6c2b7a9e10", "error": failure.Error(), "error_class": "*json.UnmarshalTypeError", "organization_id": "org_test", "project_id": "prj_test", "issue_id": "wi_test", "duration": float64(3)}},
		{"unknown tool dropped", []any{"tool", "tenant-tool-sentinel", "error_class", "sqlite_5"}, map[string]any{"error_class": "sqlite_5"}},
		{"free text class dropped", []any{"error_class", "tenant error sentinel", "correlation_id", "tenant-correlation-sentinel"}, map[string]any{}},
		{"content and secrets dropped", []any{"issue_title", "tenant content sentinel", "body", "tenant content sentinel", "comment", "tenant content sentinel", "prompt", "tenant content sentinel", "conversation_text", "tenant content sentinel", "token", "secret-sentinel", "error", fmt.Errorf("load version: token=secret-sentinel: %w", context.DeadlineExceeded)}, map[string]any{"error": "load version: [redacted] context deadline exceeded", "error_class": "deadline_exceeded"}},
		{"nested content dropped", []any{slog.Group("resource", "id", "wi_test", "body", "tenant content sentinel"), "audit", map[string]any{"project_id": "prj_test", "title": "tenant content sentinel", "token": "secret-sentinel"}}, map[string]any{"resource": map[string]any{"id": "wi_test"}, "audit": map[string]any{"project_id": "prj_test"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output hostedLogBuffer
			logger := slog.New(hostedLogHandler{output: logging.NewHandler(&output, slog.LevelInfo, false, logging.SourceSetting{})}).With("title", "tenant content sentinel", "secret", "attribute-sentinel", "component", "operator")
			logger.Error("operator tool failed", tt.attrs...)
			var record map[string]any
			if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
				t.Fatal(err)
			}
			source, ok := record["source"].(map[string]any)
			if !ok || !strings.HasSuffix(source["file"].(string), "hosted_login_log_test.go") || source["line"].(float64) <= 0 {
				t.Fatal(output.String())
			}
			if record["msg"] != "operator tool failed" || record["component"] != "operator" {
				t.Fatal(output.String())
			}
			for _, key := range []string{"time", "level", "msg", "source", "component"} {
				delete(record, key)
			}
			if !reflect.DeepEqual(record, tt.want) || strings.Contains(output.String(), "sentinel") {
				t.Fatalf("attrs=%v want=%v: %s", record, tt.want, output.String())
			}
		})
	}
}
