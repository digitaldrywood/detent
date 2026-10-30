package hubanalytics

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsAttemptProjectionFields(t *testing.T) {
	for _, extra := range []string{``, `,"new_runtime_field":{"body":"private-runtime"},"completion":{"text":"private-completion"},"worker_launch":{"metadata":"private-worker"}`} {
		raw := `{"handoff":"private-handoff","artifact_body":"private-artifact","runtime":{"phases":[],"phases_dropped":2,"validation":{"command":"make check-land","exit_code":0,"output":"private-output"},"landing":{"gate":{"output":"private-gate-output","pipeline":{"timings":[{"receipt_id":"timing_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","stage":"rebase","started_at":"2026-10-01T00:00:00Z","finished_at":"2026-10-01T00:01:00Z"}],"dropped":2}}}` + extra + `}}`
		projected, err := ProjectAttempt(raw, "attempt", "item", "lease", "completed", "2026-10-01T00:00:00Z", "2026-10-01T01:00:00Z", sql.NullString{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(projected, "private-") || strings.Contains(projected, "new_runtime_field") {
			t.Fatalf("non-report fields stored: %s", projected)
		}
		var value AttemptProjection
		if err := json.Unmarshal([]byte(projected), &value); err != nil {
			t.Fatal(err)
		}
		if value.ID != "attempt" || value.Runtime.PhasesDropped != 2 || value.Runtime.Validation == nil || value.Runtime.Validation.Command != "make check-land" {
			t.Fatalf("report fields lost: %s", projected)
		}
		var stored tracker.NativeRunData
		if err := json.Unmarshal([]byte(projected), &stored); err != nil {
			t.Fatal(err)
		}
		pipeline := stored.Runtime.Landing.Gate.Pipeline
		if pipeline == nil || pipeline.Dropped != 2 || len(pipeline.Timings) != 1 || pipeline.Timings[0].Stage != "rebase" {
			t.Fatalf("nested pipeline evidence lost: %+v", pipeline)
		}
	}
}
