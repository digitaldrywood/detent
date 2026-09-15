package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestStatusRetentionTotals(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			totals := []workspace.RetentionTotals{{Workdir: "/workdir", At: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), HookLogs: workspace.RemovalTotal{Count: count, Bytes: int64(count * 16)}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{"workspace_retention": totals}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			runner := statusServiceRunner{ServiceRunner: &serviceRunnerStub{status: servicepkg.Status{State: servicepkg.StateRunning}}, fallbackURL: server.URL, httpDo: server.Client().Do}
			status, err := runner.Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(status.WorkspaceRetention) != 1 || status.WorkspaceRetention[0].HookLogs.Count != count {
				t.Fatalf("status=%+v", status)
			}
			var out bytes.Buffer
			if err := writeServiceStatusText(&out, status); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Last workspace sweep: /workdir") || !strings.Contains(out.String(), "hook_logs=") {
				t.Fatalf("output=%s", out.String())
			}
		})
	}
}
