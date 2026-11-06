package hubserver

import (
	"database/sql"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func rebuildTestAnalyticsRollups(t testing.TB, s *Service, scope nativeScope) {
	t.Helper()
	convertTestAnalyticsProjections(t, s)
	tx, err := s.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := rebuildAnalyticsRollups(t.Context(), tx, scope, s.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func readTestRollupRows(t *testing.T, q nativeQueryer, scope nativeScope) []string {
	t.Helper()
	rows, err := q.QueryContext(t.Context(), `SELECT json_array(hour,metric,dimension,value,json(data_json)) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? ORDER BY metric,hour,dimension`, scope.organization, scope.project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		result = append(result, raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func analyticsRollupsServed(t *testing.T, s *Service, scope nativeScope) bool {
	t.Helper()
	q, err := analyticsProjectReader(t.Context(), analyticsRollupReader{s.database.reader}, scope)
	if err != nil {
		t.Fatal(err)
	}
	_, served := q.(analyticsRollupReader)
	return served
}

func commitTestAnalytics(t *testing.T, s *Service, write func(*sql.Tx)) {
	t.Helper()
	tx, err := s.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	write(tx)
	if err := s.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestAnalytics(t, s)
}

func drainTestAnalytics(t *testing.T, s *Service) {
	t.Helper()
	convertTestAnalyticsProjections(t, s)
	for {
		more, err := s.database.drainAnalyticsDirty(t.Context(), s.config.now())
		if err != nil {
			t.Log(err)
		}
		if !more {
			return
		}
	}
}

func projectTestAnalytics(t *testing.T, s *Service, tx *sql.Tx) {
	t.Helper()
	for {
		if err := s.database.projectAnalyticsBatch(t.Context(), tx, s.config.now(), analyticsBackfillBatch); err != nil {
			t.Fatal(err)
		}
		var more bool
		if err := tx.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM analytics_rollup_dirty)").Scan(&more); err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
}

func countTestAnalyticsDirty(t *testing.T, s *Service) int {
	t.Helper()
	var count int
	if err := s.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_rollup_dirty").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertAnalyticsJSONEqual(t *testing.T, want, got any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wantValue, gotValue any
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatal(err)
	}
	if !analyticsJSONClose(wantValue, gotValue) {
		t.Fatalf("response differs\nwant: %s\ngot: %s", wantJSON, gotJSON)
	}
}

func analyticsJSONClose(want, got any) bool {
	switch w := want.(type) {
	case float64:
		g, ok := got.(float64)
		return ok && math.Abs(w-g) <= 1e-9*math.Max(1, math.Max(math.Abs(w), math.Abs(g)))
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for i := range w {
			if !analyticsJSONClose(w[i], g[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for k, v := range w {
			gv, ok := g[k]
			if !ok || !analyticsJSONClose(v, gv) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

func TestAnalyticsAttemptProjectionFields(t *testing.T) {
	for _, extra := range []string{``, `,"new_runtime_field":{"body":"private-runtime"},"completion":{"text":"private-completion"},"worker_launch":{"metadata":"private-worker"}`} {
		raw := `{"handoff":"private-handoff","artifact_body":"private-artifact","runtime":{"phases":[],"phases_dropped":2,"validation":{"command":"make check-land","exit_code":0,"output":"private-output"},"landing":{"gate":{"output":"private-gate-output","pipeline":{"timings":[{"receipt_id":"timing_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","stage":"rebase","started_at":"2026-10-01T00:00:00Z","finished_at":"2026-10-01T00:01:00Z"}],"dropped":2}}}` + extra + `}}`
		projected, err := projectAnalyticsAttempt(raw, "attempt", "item", "lease", "completed", "2026-10-01T00:00:00Z", "2026-10-01T01:00:00Z", sql.NullString{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(projected, "private-") || strings.Contains(projected, "new_runtime_field") {
			t.Fatalf("non-report fields stored: %s", projected)
		}
		var value analyticsAttemptProjection
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

func convertTestAnalyticsProjections(t testing.TB, s *Service) {
	t.Helper()
	ctx := s.runDetailContext(t.Context())
	for {
		if err := s.convertActivityProjections(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.convertSchedulerDecisions(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.database.convertAnalyticsProjections(ctx); err != nil {
			t.Fatal(err)
		}
		var more bool
		if err := s.database.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE analytics_projection_version IS NULL) OR EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE projection_version IS NULL) OR EXISTS(SELECT 1 FROM native_attempts WHERE report_projection_version IS NULL)").Scan(&more); err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
}
