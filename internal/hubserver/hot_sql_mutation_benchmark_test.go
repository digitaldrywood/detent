package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func benchmarkHotSQLMutations(b *testing.B, f nativeFixture, scope nativeScope, item string, view runnerauth.ProjectConfiguration, now time.Time) {
	b.Helper()
	q := f.service.database.reader
	scope.credential.Runner.RunnerID = "bench-runner"
	prepared, err := encodeProjectConfiguration(view)
	if err != nil {
		b.Fatal(err)
	}
	for _, changed := range []bool{false, true} {
		for _, typed := range []bool{false, true} {
			b.Run(fmt.Sprintf("heartbeat/changed=%t/typed=%t", changed, typed), func(b *testing.B) {
				snapshot := prepared
				if changed {
					next := view
					next.Paused = !view.Paused
					var err error
					snapshot, err = encodeProjectConfiguration(next)
					if err != nil {
						b.Fatal(err)
					}
				}
				if _, err := f.service.database.db.ExecContext(b.Context(), `UPDATE runner_project_observations SET snapshot_digest=? WHERE organization_id=? AND runner_id=? AND project_id=?`, prepared.digest, scope.organization, scope.credential.Runner.RunnerID, scope.project); err != nil {
					b.Fatal(err)
				}
				input, err := prepareLegacyProjectConfiguration(b.Context(), q, scope, snapshot)
				if err != nil {
					b.Fatal(err)
				}
				store := storeJSONProjectConfiguration
				if typed {
					store = storePreparedProjectConfiguration
				}
				check, err := f.service.database.db.BeginTx(b.Context(), nil)
				if err != nil {
					b.Fatal(err)
				}
				if err := store(b.Context(), check, scope, input); err != nil {
					check.Rollback()
					b.Fatal(err)
				}
				raw, err := readProjectConfigurationEntry(b.Context(), check, scope, scope.credential.Runner.RunnerID)
				if err != nil || raw.String != input.raw {
					check.Rollback()
					b.Fatalf("heartbeat did not retain configuration: %v", err)
				}
				if typed {
					projected, err := readRunnerBoardProjection(b.Context(), check, scope, scope.credential.Runner.RunnerID, item)
					if err != nil || projected.Diagnostics == nil || len(projected.Diagnostics.Records) != len(view.Diagnostics.Records) {
						check.Rollback()
						b.Fatalf("heartbeat lost board diagnostic: %+v %v", projected, err)
					}
				}
				if err := check.Rollback(); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					tx, err := f.service.database.db.BeginTx(b.Context(), nil)
					if err != nil {
						b.Fatal(err)
					}
					if err := store(b.Context(), tx, scope, input); err != nil {
						tx.Rollback()
						b.Fatal(err)
					}
					if err := tx.Rollback(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
	tx, err := f.service.database.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	decision := tracker.NativeSchedulerDecision{RunnerID: "bench-runner", Source: "native_runner_admission", Outcome: "skipped", Reason: "instance_capacity"}
	for i := range 10000 {
		d := decision
		if i > 0 {
			d.RunnerID, d.Source, d.Outcome = "other-runner", "native_claim", "ready"
		}
		if err := appendNativeHistory(b.Context(), tx, scope, item, "scheduler.decision", tracker.CollaborationData{Decision: &d}, now); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	for _, path := range []struct {
		name  string
		json  func(context.Context) (any, error)
		typed func(context.Context) (any, error)
	}{
		{"workflow_rank", func(ctx context.Context) (any, error) {
			var rank int
			err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(key AS INTEGER) FROM json_each(states_json) WHERE json_extract(value,'$.name')=?),2147483647) FROM projects WHERE id=?`, "Todo", scope.project).Scan(&rank)
			return rank, err
		}, func(ctx context.Context) (any, error) {
			return readNativeBoardRank(ctx, q, string(scope.project), "Todo")
		}},
		{"scheduler_latest", func(ctx context.Context) (any, error) {
			return latestJSONRuntimeEvent(ctx, q, scope, item, "scheduler.decision", "skipped")
		}, func(ctx context.Context) (any, error) {
			return latestNativeRuntimeEvent(ctx, q, scope, item, "scheduler.decision", "skipped")
		}},
		{"scheduler_deduplication", func(ctx context.Context) (any, error) {
			return previousJSONSchedulingDecision(ctx, q, scope, item, decision)
		}, func(ctx context.Context) (any, error) {
			return previousSchedulingDecision(ctx, q, scope, item, decision)
		}},
		{"scheduler_newer", func(ctx context.Context) (any, error) {
			var found bool
			err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision' AND json_extract(data_json,'$.decision.runner_id')=? AND recorded_at>?)`, scope.organization, scope.project, item, decision.RunnerID, formatHubTime(now.Add(-time.Second))).Scan(&found)
			return found, err
		}, func(ctx context.Context) (any, error) {
			return hasNewerRunnerDecision(ctx, q, scope, item, decision.RunnerID, now.Add(-time.Second))
		}},
	} {
		want, err := path.json(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		got, err := path.typed(b.Context())
		if err != nil || !reflect.DeepEqual(got, want) {
			b.Fatalf("%s changed result: want=%+v got=%+v err=%v", path.name, want, got, err)
		}
		for _, typed := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/typed=%t", path.name, typed), func(b *testing.B) {
				read := path.json
				if typed {
					read = path.typed
				}
				b.ReportAllocs()
				for b.Loop() {
					if _, err := read(b.Context()); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func previousJSONSchedulingDecision(ctx context.Context, q nativeQueryer, scope nativeScope, item string, decision tracker.NativeSchedulerDecision) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT data_json FROM collaboration_events
WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision'
AND coalesce(json_extract(data_json, '$.decision.runner_id'), '')=?
AND ((?='skipped' AND json_extract(data_json, '$.decision.outcome')='skipped' AND json_extract(data_json, '$.decision.reason')=?)
 OR (?<>'skipped' AND json_extract(data_json, '$.decision.source')=?))
AND sequence > coalesce((SELECT max(sequence) FROM collaboration_events
 WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision'
 AND json_extract(data_json, '$.decision.outcome')='claimed'), 0)
ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, item, decision.RunnerID, decision.Outcome, decision.Reason, decision.Outcome, decision.Source, scope.organization, scope.project, item).Scan(&raw)
	return raw, err
}

func storeJSONProjectConfiguration(ctx context.Context, tx *sql.Tx, scope nativeScope, prepared preparedProjectConfiguration) error {
	view, digest := prepared.view, prepared.digest
	runner := scope.credential.Runner.RunnerID
	var previous string
	err := tx.QueryRowContext(ctx, "SELECT snapshot_digest FROM runner_project_observations WHERE organization_id = ? AND runner_id = ? AND project_id = ?", scope.organization, runner, scope.project).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) && prepared.legacyRaw.Valid {
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT json_extract(project_configuration_json, ?) FROM runner_identities WHERE organization_id = ? AND id = ?", "$."+jsonPathKey(string(scope.project)), scope.organization, runner).Scan(&raw); err != nil {
			return err
		}
		if raw == prepared.legacyRaw {
			previous = prepared.legacyDigest
		}
	}
	if previous != digest {
		if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET project_configuration_json = json_set(project_configuration_json, ?, json(?)) WHERE organization_id = ? AND id = ?", "$."+jsonPathKey(string(scope.project)), prepared.raw, scope.organization, runner); err != nil {
			return err
		}
	}
	var diagnostics, runtime any
	if view.Diagnostics != nil {
		diagnostics = formatHubTime(view.Diagnostics.ObservedAt)
		if !view.Diagnostics.RuntimeObservedAt.IsZero() {
			runtime = formatHubTime(view.Diagnostics.RuntimeObservedAt)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runner_project_observations
(organization_id, runner_id, project_id, observed_at, diagnostics_observed_at, runtime_observed_at, snapshot_digest)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (organization_id, runner_id, project_id) DO UPDATE SET
observed_at = excluded.observed_at, diagnostics_observed_at = excluded.diagnostics_observed_at,
runtime_observed_at = excluded.runtime_observed_at, snapshot_digest = excluded.snapshot_digest`,
		scope.organization, runner, scope.project, formatHubTime(view.ObservedAt), diagnostics, runtime, digest)
	return err
}

func latestJSONRuntimeEvent(ctx context.Context, q nativeQueryer, scope nativeScope, item, kind, outcome string) (tracker.CollaborationEvent, error) {
	e := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: tracker.NativeWorkItemID(item), AggregateType: "work_item"}
	var actor, data, at string
	statement := "SELECT id, sequence, type, schema_version, actor_json, data_json, recorded_at FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type=?"
	args := []any{scope.organization, scope.project, item, kind}
	if outcome != "" {
		statement += " AND json_extract(data_json, '$.decision.outcome')=?"
		args = append(args, outcome)
	}
	err := q.QueryRowContext(ctx, statement+" ORDER BY sequence DESC LIMIT 1", args...).Scan(&e.ID, &e.AggregateSequence, &e.Type, &e.SchemaVersion, &actor, &data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.CollaborationEvent{}, nil
	}
	if err != nil {
		return tracker.CollaborationEvent{}, err
	}
	if err := json.Unmarshal([]byte(actor), &e.Actor); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	if err := decodeRunHistory(ctx, data, &e.Data); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	e.RecordedAt, err = parseTimeValue(at)
	return e, err
}
