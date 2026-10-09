package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readLandingBarrier(ctx context.Context, query nativeQueryer, scope nativeScope, repository string) (tracker.LandingBarrier, error) {
	result := tracker.LandingBarrier{Repository: repository, ProjectID: scope.project}
	var raw string
	err := query.QueryRowContext(ctx, "SELECT record_json FROM landing_barriers WHERE organization_id=? AND repository=?", scope.organization, repository).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	err = json.Unmarshal([]byte(raw), &result)
	return result, err
}

func writeLandingBarrier(ctx context.Context, tx *sql.Tx, scope nativeScope, barrier tracker.LandingBarrier) error {
	raw, err := marshalNative(barrier)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO landing_barriers (organization_id, repository, project_id, record_json) VALUES (?, ?, ?, ?)
ON CONFLICT(organization_id, repository) DO UPDATE SET record_json=excluded.record_json`, scope.organization, barrier.Repository, scope.project, raw)
	return err
}

func (s *Service) getLandingBarrier(c echo.Context) error {
	result, err := readLandingBarrier(c.Request().Context(), s.database.db, nativeRequestScope(c), c.QueryParam("repository"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if result.Running {
		available, err := landingBarrierOwnerAvailable(c.Request().Context(), s.database.db, nativeRequestScope(c), result, s.config.now(), false)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		result.Running = !available
	}
	if result.ProjectID != nativeRequestScope(c).project {
		result = tracker.LandingBarrier{Repository: result.Repository, Running: result.Running, Red: result.Red}
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) mutateLandingBarrier(c echo.Context) error {
	var request tracker.LandingBarrierRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if scope.credential.Runner.RunnerID == "" && scope.credential.Scope == apiScopeWorker {
			return nil, nativeNotFound()
		}
		barrier, err := readLandingBarrier(ctx, tx, scope, request.Repository)
		if err != nil {
			return nil, err
		}
		if barrier.ProjectID != scope.project {
			return nil, nativeNotFound()
		}
		switch request.Action {
		case "start":
			approved, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
			if err != nil {
				return nil, err
			}
			if approved.Policy.ID != request.PolicyID || approved.Policy.Gates.LandingMode != gate.LandingRollingBarrier {
				return nil, nativeInvalid("Rolling landing requires the approved project policy")
			}
			if !changerequest.ValidHash(request.Head, 40) && !changerequest.ValidHash(request.Head, 64) {
				return nil, nativeInvalid("Barrier start requires the observed integration branch head")
			}
			if requester := scope.credential.Runner.RunnerID; requester != "" && (!request.Recover || !barrier.Running || barrier.Owner != requester) {
				preferred, err := preferredBarrierRunner(ctx, tx, scope, now)
				if err != nil {
					return nil, err
				}
				if preferred != "" && preferred != requester {
					return barrier, nil
				}
			}
			if barrier.Running {
				available, err := landingBarrierOwnerAvailable(ctx, tx, scope, barrier, now, request.Recover)
				if err != nil {
					return nil, err
				}
				if !available {
					return barrier, nil
				}
				barrier.Started = barrier.Green
			} else if barrier.Result != nil && barrier.Result.HeadSHA == request.Head {
				return barrier, nil
			}
			var latest int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM landing_barrier_receipts WHERE organization_id=? AND repository=? AND project_id=?", scope.organization, request.Repository, scope.project).Scan(&latest); err != nil {
				return nil, err
			}
			changes, err := barrierChanges(ctx, tx, scope, request.Repository, barrier.Green, latest)
			if err != nil {
				return nil, err
			}
			barrier.CommandDigest = approved.Policy.Gates.LandingCommandDigest
			barrier.ID = request.IdempotencyKey
			barrier.Owner = landingBarrierOwner(scope)
			barrier.Started, barrier.Running = latest, true
			barrier.Changes = changes
			if len(changes) > 0 {
				barrier.BaseRef = changes[len(changes)-1].BaseRef
			}
		case "finish", "cancel":
			if !barrier.Running || barrier.ID != request.ID || barrier.Owner != landingBarrierOwner(scope) {
				return nil, nativeConflict(0)
			}
			barrier.Running = false
			if request.Action == "cancel" {
				barrier.Started = barrier.Green
			} else {
				if request.Result == nil || policy.Digest([]byte(request.Result.Command)) != barrier.CommandDigest || request.Result.Command == "" || request.Result.ExitCode < 0 || request.Result.DurationNS < 0 || len(request.Result.Command) > 4096 || len(request.Result.Output) > 64<<10 || !request.Result.Evidence.Valid() || !validCommitID(request.Result.TreeSHA) || !changerequest.ValidHash(request.Result.HeadSHA, 40) && !changerequest.ValidHash(request.Result.HeadSHA, 64) {
					return nil, nativeInvalid("Barrier completion requires a command result and checked commit")
				}
				barrier.Result = request.Result
				barrier.Red = request.Result.ExitCode != 0
				if err := coverBarrierReceipts(ctx, tx, scope, barrier); err != nil {
					return nil, err
				}
				if barrier.Red {
					changes, err := barrierChanges(ctx, tx, scope, request.Repository, barrier.Green, 0)
					if err != nil {
						return nil, err
					}
					barrier.Changes = changes
				} else {
					barrier.Green = barrier.Started
					barrier.GreenHead = request.Result.HeadSHA
				}
				barrier.Repair = ""
			}
		default:
			return nil, nativeInvalid("Barrier action must be start, finish or cancel")
		}
		if err := writeLandingBarrier(ctx, tx, scope, barrier); err != nil {
			return nil, err
		}
		return barrier, nil
	})
}

func recordBarrierLanding(ctx context.Context, tx *sql.Tx, scope nativeScope, version tracker.ChangeVersion, change tracker.ChangeRequest, request tracker.LandChangeVersion, now time.Time) error {
	if version.Policy.Gates.LandingMode != gate.LandingRollingBarrier {
		return nil
	}
	receipt := tracker.NativeLandingReceipt{}
	if request.Receipt != nil {
		receipt = *request.Receipt
	}
	receipt.ChangeID, receipt.VersionID, receipt.HeadSHA = change.ID, version.ID, version.HeadSHA
	receipt.MergeSHA, receipt.BaseRef, receipt.Method = request.MergeSHA, request.BaseRef, request.Method
	receipt.Landed, receipt.Rebased, receipt.ObservedAt = true, request.Rebased, now
	raw, err := marshalNative(receipt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO landing_barrier_receipts (organization_id, project_id, repository, version_id, record_json) VALUES (?, ?, ?, ?, ?) ON CONFLICT(version_id) DO NOTHING`, scope.organization, scope.project, version.Repository, version.ID, raw)
	return err
}

func barrierChanges(ctx context.Context, query nativeQueryer, scope nativeScope, repository string, after, through int64) ([]tracker.NativeLandingReceipt, error) {
	rows, err := query.QueryContext(ctx, `SELECT record_json FROM landing_barrier_receipts WHERE organization_id=? AND repository=? AND sequence>? AND (?=0 OR sequence<=?) ORDER BY sequence`, scope.organization, repository, after, through, through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []tracker.NativeLandingReceipt{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var receipt tracker.NativeLandingReceipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return nil, err
		}
		receipt.Gate, receipt.Barrier = nil, nil
		result = append(result, receipt)
	}
	return result, rows.Err()
}

func coverBarrierReceipts(ctx context.Context, tx *sql.Tx, scope nativeScope, barrier tracker.LandingBarrier) error {
	raw, err := marshalNative(barrier.Result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE landing_barrier_receipts SET record_json=json_set(record_json, '$.barrier', json(?)) WHERE organization_id=? AND repository=? AND sequence>? AND sequence<=?`, raw, scope.organization, barrier.Repository, barrier.Green, barrier.Started)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE change_landing_receipts SET record_json=json_set(record_json, '$.barrier', json(?)) WHERE version_id IN (SELECT version_id FROM landing_barrier_receipts WHERE organization_id=? AND repository=? AND sequence>? AND sequence<=?)`, raw, scope.organization, barrier.Repository, barrier.Green, barrier.Started)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE native_attempts SET data_json=json_set(data_json, '$.runtime.landing', json((SELECT record_json FROM change_landing_receipts WHERE attempt_id=native_attempts.id))) WHERE id IN (SELECT attempt_id FROM change_landing_receipts WHERE version_id IN (SELECT version_id FROM landing_barrier_receipts WHERE organization_id=? AND repository=? AND sequence>? AND sequence<=?))`, scope.organization, barrier.Repository, barrier.Green, barrier.Started)
	return err
}

func landingBarrierOwner(scope nativeScope) string {
	if scope.credential.Runner.RunnerID != "" {
		return scope.credential.Runner.RunnerID
	}
	return scope.credential.ID
}

func preferredBarrierRunner(ctx context.Context, query nativeQueryer, scope nativeScope, now time.Time) (string, error) {
	rows, err := query.QueryContext(ctx, `SELECT DISTINCT r.id FROM runner_identities r JOIN token_grants g ON g.token_id = r.token_id WHERE r.organization_id = ? AND g.project_id = ? ORDER BY r.id`, scope.organization, scope.project)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	preferred, capacity := "", -1
	for _, id := range ids {
		runner, err := readRunner(ctx, query, scope.organization, id, now)
		if err != nil {
			return "", err
		}
		if runner.Health != "online" || runner.State == "disabled" || runner.HostCapacity <= capacity {
			continue
		}
		preferred, capacity = id, runner.HostCapacity
	}
	return preferred, nil
}

func landingBarrierOwnerAvailable(ctx context.Context, query nativeQueryer, scope nativeScope, barrier tracker.LandingBarrier, now time.Time, recoverClaim bool) (bool, error) {
	if recoverClaim && barrier.Owner == landingBarrierOwner(scope) {
		return true, nil
	}
	if !strings.HasPrefix(barrier.Owner, "runner_") {
		return false, nil
	}
	owner, err := readRunner(ctx, query, scope.organization, barrier.Owner, now)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return owner.Health != "online" || owner.State == "disabled", err
}
