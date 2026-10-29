package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerFactsRowsPerTick = 100

type runnerFactBackfill struct {
	verify string
	args   []any
	write  func(context.Context, *sql.Tx) error
}

func (d *database) populateRunnerFacts(ctx context.Context) error {
	for range runnerFactsRowsPerTick {
		more, err := d.populateRunnerFact(ctx)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func (d *database) populateRunnerFact(ctx context.Context) (bool, error) {
	candidate, err := prepareRunnerFactBackfill(ctx, d.reader)
	if err != nil || candidate.write == nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var current bool
	if err := tx.QueryRowContext(ctx, candidate.verify, candidate.args...).Scan(&current); err != nil {
		return false, err
	}
	if !current {
		return true, nil
	}
	if err := candidate.write(ctx, tx); err != nil {
		return false, err
	}
	return true, d.commit(ctx, tx)
}

func prepareRunnerFactBackfill(ctx context.Context, q nativeQueryer) (runnerFactBackfill, error) {
	var id, raw, extra string
	err := q.QueryRowContext(ctx, `SELECT id,capabilities_json FROM machines WHERE machine_facts_ready IS NULL OR admission_facts_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
	if err == nil {
		return prepareMachineFactBackfill(id, raw)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return runnerFactBackfill{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT id,routing_settings_json,local_checks_json FROM runner_identities WHERE routing_facts_ready IS NULL OR local_check_facts_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw, &extra)
	if err == nil {
		return prepareRoutingFactBackfill(id, raw, extra)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return runnerFactBackfill{}, err
	}
	var row int64
	err = q.QueryRowContext(ctx, `SELECT rowid,response_json FROM native_commands WHERE response_pending IS NULL ORDER BY rowid LIMIT 1`).Scan(&row, &raw)
	if err == nil {
		pending, request, err := commandResponseFacts(raw)
		return runnerFactBackfill{verify: `SELECT EXISTS(SELECT 1 FROM native_commands WHERE rowid=? AND response_json=? AND response_pending IS NULL)`, args: []any{row, raw}, write: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE native_commands SET response_pending=?,response_request_id=? WHERE rowid=?`, pending, request, row)
			return err
		}}, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return runnerFactBackfill{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT id,data_json FROM native_attempts WHERE usage_facts_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return runnerFactBackfill{}, nil
	}
	if err != nil {
		return runnerFactBackfill{}, err
	}
	var data tracker.NativeRunData
	if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
		return runnerFactBackfill{}, err
	}
	return runnerFactBackfill{verify: `SELECT EXISTS(SELECT 1 FROM native_attempts WHERE id=? AND data_json=? AND usage_facts_ready IS NULL)`, args: []any{id, raw}, write: func(ctx context.Context, tx *sql.Tx) error { return writeAttemptUsageFacts(ctx, tx, id, data) }}, nil
}

func prepareMachineFactBackfill(id, raw string) (runnerFactBackfill, error) {
	var facts machineSpriteFacts
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return runnerFactBackfill{}, err
	}
	var payload struct {
		Admission map[string]map[tracker.ProjectID]json.RawMessage `json:"native_admission"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return runnerFactBackfill{}, err
	}
	observations := make(map[string]map[tracker.ProjectID]tracker.NativeAdmissionObservation)
	for runner, projects := range payload.Admission {
		observations[runner] = make(map[tracker.ProjectID]tracker.NativeAdmissionObservation)
		for project, raw := range projects {
			var observation tracker.NativeAdmissionObservation
			if len(raw) <= 8192 {
				if err := json.Unmarshal(raw, &observation); err != nil {
					observation = tracker.NativeAdmissionObservation{}
				}
			}
			observations[runner][project] = observation
		}
	}
	return runnerFactBackfill{verify: `SELECT EXISTS(SELECT 1 FROM machines WHERE id=? AND capabilities_json=? AND (machine_facts_ready IS NULL OR admission_facts_ready IS NULL))`, args: []any{id, raw}, write: func(ctx context.Context, tx *sql.Tx) error {
		for runner, projects := range observations {
			var valid bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runner_identities WHERE id=? AND machine_id=?)`, runner, id).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				continue
			}
			for project, observation := range projects {
				if err := storeAdmissionFacts(ctx, tx, runner, project, observation); err != nil {
					return err
				}
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE machines SET sprite_name=?,sprite_woken_at=?,sprite_wake_failed_at=?,machine_facts_ready=1,admission_facts_ready=1 WHERE id=?`, facts.Name, facts.WokenAt, facts.WakeFailedAt, id)
		return err
	}}, nil
}

func prepareRoutingFactBackfill(id, raw, checksRaw string) (runnerFactBackfill, error) {
	var settings runnerSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return runnerFactBackfill{}, err
	}
	var projects map[tracker.ProjectID]runnerauth.LocalChecks
	if err := json.Unmarshal([]byte(checksRaw), &projects); err != nil {
		return runnerFactBackfill{}, err
	}
	return runnerFactBackfill{verify: `SELECT EXISTS(SELECT 1 FROM runner_identities WHERE id=? AND routing_settings_json=? AND local_checks_json=? AND (routing_facts_ready IS NULL OR local_check_facts_ready IS NULL))`, args: []any{id, raw, checksRaw}, write: func(ctx context.Context, tx *sql.Tx) error {
		for project, checks := range projects {
			if err := storeLocalCheckFacts(ctx, tx, id, project, checks); err != nil {
				return err
			}
		}
		if err := writeRunnerRoutingFacts(ctx, tx, id, settings); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE runner_identities SET local_check_facts_ready=1 WHERE id=?`, id)
		return err
	}}, nil
}
