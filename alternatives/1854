package hubserver

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeBoardExecution(ctx context.Context, q nativeQueryer, scope nativeScope, item string, latest *tracker.NativeAttempt, now time.Time) (*tracker.NativeBoardExecution, error) {
	var lease, acquired, expires, runner, configuration string
	var fence uint64
	err := q.QueryRowContext(ctx, `SELECT l.lease_id,l.fencing_token,l.acquired_at,l.expires_at,coalesce(r.id,''),coalesce(json_extract(r.project_configuration_json,?), '{}')
FROM issues i JOIN leases l ON l.issue_id=i.id
LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=i.organization_id
AND EXISTS(SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.project_id=i.project_id)
WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND l.released_at IS NULL
AND julianday(l.renewed_at)<=julianday(?) AND julianday(l.expires_at)>julianday(?)`, "$."+jsonPathKey(string(scope.project)), scope.organization, scope.project, item, formatHubTime(now), formatHubTime(now)).Scan(&lease, &fence, &acquired, &expires, &runner, &configuration)
	if err != nil {
		return nil, err
	}
	started, err := parseTimeValue(acquired)
	if err != nil {
		return nil, err
	}
	expiry, err := parseTimeValue(expires)
	if err != nil {
		return nil, err
	}
	execution := &tracker.NativeBoardExecution{ID: lease, RunnerID: runner, ExpiresAt: expiry}
	terminal := false
	if latest != nil && string(latest.LeaseID) == lease {
		execution.ID, execution.Running, execution.Identity, execution.StartedAt = latest.AttemptID, latest.Status == "running", nativeAttemptDisplayIdentity(*latest), &latest.StartedAt
		terminal = latest.Status != "running"
		if latest.Runtime != nil {
			execution.Stage = latest.Runtime.Phase
			if execution.Stage == "completed" {
				execution.Running, execution.Stage, terminal = false, "", true
			}
		}
		if terminal {
			execution.Running, execution.Stage = false, ""
		}
	}
	var view runnerauth.ProjectConfiguration
	if err := json.Unmarshal([]byte(configuration), &view); err != nil {
		return nil, err
	}
	d := view.Diagnostics
	if d == nil || view.ProjectID != string(scope.project) || d.ObservedAt.Before(started) || d.ObservedAt.After(now) {
		return execution, nil
	}
	asOf := d.RuntimeObservedAt
	if asOf.IsZero() {
		asOf = d.ObservedAt
	}
	fresh := !asOf.Before(started) && !asOf.After(now) && now.Sub(asOf) <= runnerauth.HeartbeatTimeout && now.Sub(d.ObservedAt) <= runnerauth.HeartbeatTimeout
	var local *runnerauth.DiagnosticAttempt
	var claim *runnerauth.DiagnosticAttempt
	count := 0
	for i := range d.Records {
		record := &d.Records[i]
		if record.IssueID != item || record.Fence != 0 && record.Fence != fence {
			continue
		}
		if record.Fence == fence && slices.Contains(record.Membership, "native_claim") {
			claim = record
		}
		if record.LocalAttemptID > 0 && record.Generation > 0 && slices.Contains(record.Membership, "running") && !record.LeaseRenewedAt.Before(started) && (latest == nil || string(latest.LeaseID) != lease || record.NativeAttemptID == "" || record.NativeAttemptID == latest.AttemptID) {
			local = record
			count++
		}
	}
	if count == 1 && local != nil && !terminal && fresh {
		execution.ID = runner + "/" + strconv.FormatInt(local.LocalAttemptID, 10) + "/" + strconv.FormatUint(local.Generation, 10)
		if local.NativeAttemptID != "" {
			execution.ID = local.NativeAttemptID
		} else if claim != nil && claim.NativeAttemptID != "" {
			execution.ID = claim.NativeAttemptID
		}
		execution.Running, execution.Stage = true, local.Stage
		execution.ExpiresAt = minTime(execution.ExpiresAt, asOf.Add(runnerauth.HeartbeatTimeout))
	}
	if count != 1 {
		local = nil
	}
	completed := local != nil && !local.ProviderCompletedAt.Before(started) && !local.ProviderCompletedAt.After(now) || claim != nil && !claim.ProviderCompletedAt.Before(started) && !claim.ProviderCompletedAt.After(now)
	if completed {
		execution.Running, execution.Stage = false, ""
		if fresh && (claim != nil && claim.OperationPending && claim.HostOperation == "workspace.finalize_native_work" || local != nil && local.OperationPending && local.HostOperation == "workspace.finalize_native_work") {
			execution.Stage = "finalizing"
		}
	}
	return execution, nil
}

func nativeBoardExecutionLabel(stage string) string {
	switch stage {
	case "implementation", "implementing", "code":
		return "Implementing"
	case "rework", "reworking":
		return "Reworking"
	case "planning", "plan":
		return "Planning"
	case "validation", "validating", "review", "security_audit":
		return "Validating"
	case "merging", "merge":
		return "Merging"
	case "finalizing":
		return "Finalizing"
	default:
		return ""
	}
}
