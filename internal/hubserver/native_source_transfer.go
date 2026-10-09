package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type sourceTransferRequest struct {
	tracker.Mutation
	ExpectedRevision    tracker.Revision `json:"expected_revision,string"`
	VersionID           string           `json:"version_id"`
	DestinationRunnerID string           `json:"destination_runner_id"`
}

type sourceRecoveryView = tracker.NativeSourceRecovery
type sourceRecoveryRunner = tracker.NativeSourceRecoveryRunner

func readSourceRecoveryView(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (sourceRecoveryView, tracker.WorkItemID, error) {
	issue, id, err := readNativeIssue(ctx, q, scope, item)
	view := sourceRecoveryView{Destinations: []sourceRecoveryRunner{}, WorkItemID: issue.WorkItemID, Revision: issue.Revision, Reason: "Capture the exact legacy source on its owning runner before transfer; unavailable copies cannot be recovered"}
	if err != nil {
		return view, id, err
	}
	var routedVersion string
	if err := q.QueryRowContext(ctx, "SELECT recovery_runner_id,recovery_version_id FROM issues WHERE id=?", id).Scan(&view.DestinationRunnerID, &routedVersion); err != nil {
		return view, id, err
	}
	source, err := readNativeSourceCheckpoint(ctx, q, scope, item)
	if err != nil {
		return view, id, err
	}
	view.AttemptID, view.SourceMachineID, view.SourceRunnerID, view.SourceRunnerName = source.AttemptID, source.MachineID, source.RunnerID, source.RunnerName
	rawStatus, released := source.Status, source.Released
	uncertainEffect := source.Checkpoint.UncertainForgeEffect()
	if source.Checkpoint != nil {
		view.HeadSHA = source.Checkpoint.HeadSHA
	}

	change, found, err := readLatestNativeChangeRequest(ctx, q, scope, item)
	if err != nil || !found || change.CurrentVersion == "" {
		return view, id, err
	}
	version, err := readChangeVersion(ctx, q, change.ID, change.CurrentVersion)
	if err != nil {
		return view, id, err
	}
	view.VersionID, view.BaseSHA = version.ID, version.BaseSHA
	if routedVersion != version.ID {
		view.DestinationRunnerID = ""
	}
	if view.AttemptID == "" {
		view.HeadSHA, view.AttemptID = version.HeadSHA, version.AttemptID
		err = q.QueryRowContext(ctx, `SELECT l.machine_id, COALESCE(lr.runner_id,''),COALESCE(NULLIF(r.display_name,''),m.hostname,''), a.status, l.released_at FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=a.organization_id LEFT JOIN machines m ON m.id=l.machine_id AND m.organization_id=a.organization_id WHERE a.id=? AND a.organization_id=? AND a.project_id=? AND a.work_item_id=?`, version.AttemptID, scope.organization, scope.project, item).Scan(&view.SourceMachineID, &view.SourceRunnerID, &view.SourceRunnerName, &rawStatus, &released)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return view, id, err
		}
	}
	view.Quiesced = released.Valid && (rawStatus == "succeeded" || rawStatus == "failed" || rawStatus == "cancelled")
	var unquiesced int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM leases l JOIN issues i ON i.id=l.issue_id LEFT JOIN native_attempts a ON a.lease_id=l.lease_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND (a.status='running' OR l.released_at IS NULL)`, scope.organization, scope.project, item).Scan(&unquiesced); err != nil {
		return view, id, err
	}
	view.Quiesced = view.Quiesced && unquiesced == 0 && !uncertainEffect && !issue.Archived && !issue.Terminal
	allowed, reason, err := nativeSourceClaimAllowed(ctx, q, scope, "", id, time.Time{})
	if err != nil {
		return view, id, err
	}
	view.Available = version.Source != nil && allowed
	if view.Available {
		view.HeadSHA = version.HeadSHA
	}
	if issue.Archived || issue.Terminal {
		view.Reason = "Restore or explicitly reopen this work item before changing recovery ownership"
	} else if uncertainEffect {
		view.Reason = "Reconcile the source runner's uncertain Git push or PR creation before transfer; a stopped process does not prove its publication outcome"
	} else if !view.Available {
		view.Reason = reason
	} else if !view.Quiesced {
		view.Reason = "Finish or cancel work on the source runner and release its lease before transfer; expiry or an unreachable owner does not prove quiescence"
	} else {
		view.Reason = "Verified retained source can be restored before continuing; workflow, human and delivery holds remain in force"
	}
	return view, id, nil
}

func (s *Service) getNativeSourceRecovery(c echo.Context) error {
	ctx, scope := c.Request().Context(), nativeRequestScope(c)
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	view, _, err := readSourceRecoveryView(ctx, tx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	var refusal *nativeError
	if err != nil && (!errors.As(err, &refusal) || refusal.Code != "policy_mismatch") {
		return s.nativeAPIError(c, err)
	}
	if err == nil {
		runners, err := readPlacementRunners(ctx, tx, scope, s.config.now())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		for _, r := range runners {
			if placementRunnerCompatible(r, scope.project, approval.Policy.Requirements, s.config.now(), false) && validateReadRunnerIsolation(ctx, tx, scope, r.Runner) == nil {
				view.Destinations = append(view.Destinations, sourceRecoveryRunner{ID: r.RunnerID, Name: r.DisplayName})
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) transferNativeSource(c echo.Context) error {
	var request sourceTransferRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.transferNativeSourceCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/json", result)
}

func (s *Service) transferNativeSourceCommand(ctx context.Context, scope nativeScope, item string, request sourceTransferRequest) (json.RawMessage, error) {
	options := nativeCommandOptions{OperationID: "source_recovery " + string(scope.organization) + "/" + string(scope.project) + "/" + item, Item: item, Feature: "collaboration", RunnerAdministration: true}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		view, id, err := readSourceRecoveryView(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if request.ExpectedRevision <= 0 || request.ExpectedRevision != view.Revision {
			return nil, nativeConflict(view.Revision)
		}
		if request.VersionID == "" || request.VersionID != view.VersionID {
			return nil, nativeInvalid("Re-read the current source version before transfer")
		}
		if !view.Quiesced {
			return nil, nativeInvalid(view.Reason)
		}
		if request.DestinationRunnerID != "" {
			if !view.Available {
				return nil, nativeInvalid(view.Reason)
			}
			approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
			if err != nil {
				return nil, err
			}
			runners, err := readPlacementRunners(ctx, tx, scope, now)
			if err != nil {
				return nil, err
			}
			var destination *placementRunner
			for i := range runners {
				if runners[i].RunnerID == request.DestinationRunnerID {
					destination = &runners[i]
					break
				}
			}
			if destination == nil || !placementRunnerCompatible(*destination, scope.project, approval.Policy.Requirements, now, false) {
				return nil, nativeInvalid("Destination runner must be online, eligible and have available capacity")
			}
			if err := validateReadRunnerIsolation(ctx, tx, scope, destination.Runner); err != nil {
				return nil, nativeInvalid("Destination runner must support its configured isolation tier")
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE issues SET recovery_runner_id=?,recovery_version_id=?,revision=revision+1,native_updated_at=?,updated_at=? WHERE id=?`, request.DestinationRunnerID, view.VersionID, formatHubTime(now), formatHubTime(now), id)
		if err != nil {
			return nil, err
		}
		if err := requestNativeDispatch(ctx, tx, scope, item, now); err != nil {
			return nil, err
		}
		view.DestinationRunnerID, view.Revision = request.DestinationRunnerID, view.Revision+1
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		audit, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		if err := recordNativeChange(ctx, tx, scope, issue, item, view.Revision, "issue.edited", tracker.CollaborationData{Revision: view.Revision, Fields: []string{"recovery_runner_id"}, Operation: "source_recovery", ReasonDetail: string(audit)}, now); err != nil {
			return nil, err
		}
		return view, nil
	})
}
