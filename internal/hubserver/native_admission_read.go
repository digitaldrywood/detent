package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateNativeAdmissionContext(c tracker.NativeAdmissionContext) error {
	if err := c.Validate(); err != nil {
		return nativeInvalid(err.Error())
	}
	return nil
}

func admissionObservationPath(runner string, project tracker.ProjectID) string {
	return "$.native_admission." + runner + "." + string(project)
}

func storeRunnerAdmissionObservation(ctx context.Context, tx *sql.Tx, scope nativeScope, observation *tracker.NativeAdmissionObservation, now time.Time) error {
	if observation == nil {
		return nil
	}
	if observation.RunnerRevision < 1 || !observation.ReceivedAt.IsZero() {
		return nativeInvalid("Admission observation requires the observed runner revision")
	}
	if err := validateNativeAdmissionContext(observation.Context); err != nil {
		return err
	}
	current := *observation
	current.ReceivedAt = now
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if len(raw) > 8192 {
		return nativeInvalid("Admission observation exceeds its bound")
	}
	result, err := tx.ExecContext(ctx, "UPDATE machines SET capabilities_json=json_set(capabilities_json, ?, json(?)) WHERE id=? AND organization_id=?", admissionObservationPath(scope.credential.Runner.RunnerID, scope.project), string(raw), scope.credential.Runner.MachineID, scope.organization)
	return requireRunnerUpdate(result, err)
}

func readRunnerAdmissionObservation(ctx context.Context, q nativeQueryer, scope nativeScope, runner runnerauth.Runner, policyID string, now time.Time) (*tracker.NativeAdmissionContext, error) {
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(json_extract(capabilities_json, ?), '') FROM machines WHERE id=? AND organization_id=?", admissionObservationPath(runner.RunnerID, scope.project), runner.MachineID, scope.organization).Scan(&raw); err != nil {
		return nil, err
	}
	if raw == "" || len(raw) > 8192 {
		return nil, nil //nolint:nilnil // Missing or unusable heartbeat evidence is a valid optional result.
	}
	var observed tracker.NativeAdmissionObservation
	if err := json.Unmarshal([]byte(raw), &observed); err != nil {
		return nil, nil //nolint:nilnil // Missing or unusable heartbeat evidence is a valid optional result.
	}
	if validateNativeAdmissionContext(observed.Context) != nil || observed.RunnerRevision != runner.Revision || observed.Context.PolicyID != policyID || observed.ReceivedAt.IsZero() || now.Before(observed.ReceivedAt) || !now.Before(observed.ReceivedAt.Add(runnerauth.HeartbeatTimeout)) || now.Before(observed.Context.ObservedAt) || !now.Before(observed.Context.ObservedAt.Add(runnerauth.HeartbeatTimeout)) {
		return nil, nil //nolint:nilnil // Missing or unusable heartbeat evidence is a valid optional result.
	}
	return &observed.Context, nil
}

func readNativeAdmission(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID, policyID string, requirements policy.Requirements, runners []runnerauth.Runner, truncated, ready bool, evidence *tracker.NativeRuntimeEvidence, contexts []tracker.NativeAdmissionContext, minimumVersion, nonExecutableReason string) error {
	now := evidence.ObservedAt
	query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), WorkItemID: id, AvailableAt: now, Limit: 1}
	ids, err := nativeCandidateIDs(ctx, q, query, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		return err
	}
	var supplied *tracker.NativeAdmissionContext
	if len(contexts) > 0 {
		supplied = &contexts[0]
	}
	for _, r := range runners {
		selected, err := readRunnerAdmissionObservation(ctx, q, scope, r, policyID, now)
		if err != nil {
			return err
		}
		selectorSource := "registered_runner_heartbeat"
		if supplied != nil && scope.credential.Runner.RunnerID == r.RunnerID {
			selected = supplied
			selectorSource = "registered_runner_published_context"
		}
		a := tracker.NativeRuntimeAdmission{RunnerID: r.RunnerID, RunnerRevision: r.Revision, PolicyID: policyID, Source: "native_claim_candidate_snapshot", ObservedAt: now, Outcome: "unknown", Reason: "Current native candidate predicates match; runner request selectors are unavailable", Unavailable: []string{}}
		refuse := func(code, reason string) {
			a.Outcome, a.ReasonCode, a.Reason = "skipped", code, reason
		}
		switch {
		case nonExecutableReason != "":
			refuse("inactive_state", nonExecutableReason)
		case len(ids) == 0:
			refuse("no_claimable_work", "The existing native claim candidate query does not select this item")
		case !ready:
			refuse("no_claimable_work", "Current Change version is not ready for native landing")
		default:
			exclusions := r.Exclusions(scope.project, requirements, false)
			if len(exclusions) > 0 {
				refuse(exclusions[0].Code, exclusions[0].Message)
			} else if selected == nil {
				a.Unavailable = append(a.Unavailable, "runner_specific_candidate_selection")
			} else if selected.PolicyID != policyID {
				refuse("policy_mismatch", "Runner policy is missing or stale; load the approved repository definition and permitted local overrides before claiming work")
			} else {
				a.SelectorSource = selectorSource
				a.SelectorObservedAt = &selected.ObservedAt
				selectedIDs, err := nativeCandidateIDs(ctx, q, query, nil, nil, normalizedQueryStrings(selected.WorkflowStates), normalizedQueryStrings(selected.Authors), normalizedQueryStrings(selected.Assignees), normalizedQueryStrings(selected.LabelInclude), normalizedQueryStrings(selected.LabelExclude))
				if err != nil {
					return err
				}
				if len(selectedIDs) == 0 || len(selected.WorkflowStates) == 0 {
					refuse("no_claimable_work", "Current registered-runner claim selectors do not select this item")
				} else {
					var version string
					if err := q.QueryRowContext(ctx, "SELECT version FROM machines WHERE id=?", r.MachineID).Scan(&version); err != nil {
						return err
					}
					if err := runnerVersionError(minimumVersion, version); err != nil {
						var failure *nativeError
						if !errors.As(err, &failure) {
							return err
						}
						refuse(failure.Code, failure.Message)
					} else if err := validateReadRunnerIsolation(ctx, q, scope, r); err != nil {
						if !errors.Is(err, ErrNoClaimableWork) {
							return err
						}
						refuse("no_claimable_work", "Runner does not report support for its configured isolation tier")
					} else {
						a.Reason = "Current registered-runner claim selectors, approved routing and capacity permit this native candidate"
						if len(r.ProviderCapacity) > 0 {
							a.Unavailable = append(a.Unavailable, "provider_candidate_requirement")
						}
						if len(r.HomeProjectIDs) > 0 {
							a.Unavailable = append(a.Unavailable, "runner_home_candidate_selection")
						}
						if len(a.Unavailable) == 0 {
							a.Outcome = "ready"
						} else {
							a.Reason += "; local provider or home selection remains unavailable"
						}
					}
				}
			}
		}
		evidence.Admission = append(evidence.Admission, a)
	}
	if len(evidence.Admission) == 0 {
		evidence.Unavailable = append(evidence.Unavailable, "runner_specific_candidate_selection")
		return nil
	}
	for _, a := range evidence.Admission {
		for _, name := range a.Unavailable {
			if !slices.Contains(evidence.Unavailable, name) {
				evidence.Unavailable = append(evidence.Unavailable, name)
			}
		}
	}
	if evidence.Scheduling.Outcome == "skipped" {
		return nil
	}
	evidence.Scheduling.Source = "native_claim_candidate_snapshot"
	if slices.ContainsFunc(evidence.Admission, func(a tracker.NativeRuntimeAdmission) bool { return a.Outcome == "ready" }) {
		evidence.Scheduling.Outcome = "ready"
		evidence.Scheduling.Reason = "A current registered runner permits this native candidate; this snapshot is not a lease grant or dispatch decision"
	} else if !truncated && !slices.ContainsFunc(evidence.Admission, func(a tracker.NativeRuntimeAdmission) bool { return a.Outcome == "unknown" }) {
		evidence.Scheduling.Outcome = "skipped"
		evidence.Scheduling.Reason = "Current enrolled runners refuse this native candidate"
	} else {
		evidence.Scheduling.Reason = "Current native admission is only partially observed; see per-runner evidence and unavailable predicates"
	}
	return nil
}
