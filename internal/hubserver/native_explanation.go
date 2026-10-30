package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/operatorfailure"
	"github.com/digitaldrywood/detent/internal/spritenames"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type explanationReadKey struct{}

func explanationRead(ctx context.Context) bool {
	value, ok := ctx.Value(explanationReadKey{}).(bool)
	return ok && value
}

func readRuntimeChangeDetail(ctx context.Context, q nativeQueryer, scope nativeScope, change tracker.ChangeRequest, now time.Time) (tracker.ChangeDetail, error) {
	if !explanationRead(ctx) {
		return readCurrentChangeDetailWithVersion(ctx, q, scope, change, now)
	}
	detail := tracker.ChangeDetail{Change: change}
	version, found, err := readNativeChangeVersion(ctx, q, change)
	if err != nil {
		return detail, err
	}
	if found {
		detail.Versions = []tracker.ChangeVersion{version}
		detail.Reviews, err = changeRows[tracker.ChangeReview](ctx, q, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' ORDER BY sequence DESC LIMIT 65", change.ID, version.ID)
		if err != nil {
			return detail, err
		}
		slices.Reverse(detail.Reviews)
		detail.Checks, err = changeRows[tracker.ChangeCheck](ctx, q, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND version_id=? AND kind='check' ORDER BY sequence DESC LIMIT 65", change.ID, version.ID)
		if err != nil {
			return detail, err
		}
		slices.Reverse(detail.Checks)
	}
	current, err := readChangeSummaryPolicy(ctx, q, scope)
	if err != nil {
		return detail, err
	}
	detail.Summary = current.summarize(detail, now, false)
	if len(detail.Checks) > 64 || len(detail.Reviews) > 64 {
		detail.Summary.Status = "evidence_incomplete"
		if len(detail.Checks) > 64 {
			detail.Summary.Checks = "unknown"
		}
		if len(detail.Reviews) > 64 {
			detail.Summary.NativeReview, detail.Summary.ExternalReview = "unknown", "unknown"
		}
		detail.Summary.Messages = []string{"Current-version check or review history is truncated"}
	}
	return detail, nil
}

func readExplanationCheckpoint(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (nativeSourceCheckpoint, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,l.machine_id,COALESCE(lr.runner_id,''),COALESCE(NULLIF(r.display_name,''),m.hostname,''),a.status,l.released_at,a.checkpoint_json,coalesce(a.failure_stage,'')='lease_release' AND a.cleanup_acknowledged_at IS NULL
FROM (
 SELECT * FROM (SELECT id,lease_id,organization_id,status,COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) AS checkpoint_json,fencing_token,failure_stage,cleanup_acknowledged_at FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND checkpoint_preserves_source=1 ORDER BY fencing_token DESC LIMIT 1)
 UNION ALL
 SELECT * FROM (SELECT id,lease_id,organization_id,status,COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) AS checkpoint_json,fencing_token,failure_stage,cleanup_acknowledged_at FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND checkpoint_preserves_source IS NULL ORDER BY fencing_token DESC LIMIT 64)
) a
JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=a.organization_id LEFT JOIN machines m ON m.id=l.machine_id AND m.organization_id=a.organization_id
ORDER BY a.fencing_token DESC`, scope.organization, scope.project, item, scope.organization, scope.project, item)
	if err != nil {
		return nativeSourceCheckpoint{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var source nativeSourceCheckpoint
		var raw sql.NullString
		if err := rows.Scan(&source.AttemptID, &source.MachineID, &source.RunnerID, &source.RunnerName, &source.Status, &source.Released, &raw, &source.UnacknowledgedRelease); err != nil {
			return source, err
		}
		if !raw.Valid {
			continue
		}
		source.Checkpoint, err = decodeRunCheckpoint(ctx, q, raw.String)
		if err != nil {
			return source, err
		}
		if source.Checkpoint.PreservesSource() {
			return source, nil
		}
	}
	return nativeSourceCheckpoint{}, rows.Err()
}

func readExplanationFailures(ctx context.Context, q nativeQueryer, scope nativeScope, item string) ([]nativeFailure, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,status,started_at,updated_at,COALESCE(NULLIF(payload_object_key,''),data_json),health_failure_signature FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT 65`, scope.organization, scope.project, item)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var failures []nativeFailure
	observed := 0
	for rows.Next() {
		if observed == 64 {
			return failures, true, rows.Err()
		}
		observed++
		var f nativeFailure
		var status, started, updated, raw, signature string
		if err := rows.Scan(&f.attemptID, &status, &started, &updated, &raw, &signature); err != nil {
			return nil, false, err
		}
		var data tracker.NativeRunData
		if err := decodeStorageJSON(ctx, []byte(raw), &data); err != nil {
			return nil, false, err
		}
		f.example, f.class = operatorfailure.FromAttempt(data, status)
		if signature != "" {
			f.example = signature
		}
		if f.example == "" {
			continue
		}
		f.workItem = item
		f.started, err = parseTimeValue(started)
		if err != nil {
			return nil, false, err
		}
		f.finished, err = parseTimeValue(updated)
		if err != nil {
			return nil, false, err
		}
		f.at = f.finished
		if data.Runtime != nil && data.Runtime.Landing != nil && !data.Runtime.Landing.ObservedAt.IsZero() {
			f.at = data.Runtime.Landing.ObservedAt
		}
		failures = append(failures, f)
	}
	return failures, false, rows.Err()
}

func explanationBudgetAvailable(ctx context.Context, q nativeQueryer, scope nativeScope) (bool, error) {
	project, err := readMonthlyBudgetSettings(ctx, q, scope)
	if err != nil {
		return false, err
	}
	scope.project = ""
	organization, err := readMonthlyBudgetSettings(ctx, q, scope)
	return !project.Policy.Enabled && !organization.Policy.Enabled, err
}

func explanationPlacementAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID) (bool, string, error) {
	if allowed, reason, err := nativeSourceClaimAllowed(ctx, q, scope, machine, id); err != nil || !allowed {
		return allowed, reason, err
	}
	allowed, err := spriteAllocationRunnerAllowed(ctx, q, scope, machine)
	if err != nil || !allowed {
		return allowed, "Project Sprite allocation excludes this runner", err
	}
	placement, _, err := readPlacementPolicy(ctx, q, scope)
	if err != nil {
		return false, "", err
	}
	var raw, hostname string
	if err := q.QueryRowContext(ctx, "SELECT capabilities_json,hostname FROM machines WHERE id=? AND organization_id=?", machine, scope.organization).Scan(&raw, &hostname); err != nil {
		return false, "", err
	}
	var capabilities struct {
		SpriteName string `json:"sprite_name"`
	}
	if err := decodeStorageJSON(ctx, []byte(raw), &capabilities); err != nil {
		return false, "", err
	}
	if capabilities.SpriteName != hostname || !spritenames.ValidSlug(hostname) {
		return placement.Mode != "sprites_only", "Resolved placement " + placement.Mode + " governs local eligibility", nil
	}
	return true, "placement_capacity_unavailable", nil
}

func explanationRun(data tracker.NativeRunData) tracker.NativeRunData {
	result := compactRun(data)
	runtime := data.Runtime.WithoutActivitySpans()
	if runtime != nil && result.Runtime != nil {
		result.Runtime.Landing = explanationLanding(runtime.Landing)
	}
	result.ArtifactIDs = result.ArtifactIDs[:min(len(result.ArtifactIDs), 16)]
	return result
}

func readAdmissionLandingCapacity(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID, evidence *tracker.NativeRuntimeEvidence, now time.Time) (bool, int, error) {
	if !explanationRead(ctx) {
		return nativeLandingClaimCapacity(ctx, q, id, nativeLandingCapacityUnknown, now)
	}
	if evidence.Issue.State != "Merging" && (evidence.Issue.ListChange == nil || !evidence.Issue.ListChange.LandingPending) {
		return true, 0, nil
	}
	var owners int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT id FROM runner_identities WHERE organization_id=? ORDER BY id LIMIT 65)", scope.organization).Scan(&owners); err != nil {
		return false, 0, err
	}
	if owners > 64 {
		return false, nativeLandingCapacityUnknown, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT l.landing_capacity,i.organization_id,i.project_id,COALESCE(w.detent_state,'') FROM
(SELECT issue_id,landing_capacity FROM leases INDEXED BY health_active_host WHERE machine_id IN (SELECT machine_id FROM runner_identities WHERE organization_id=? ORDER BY id LIMIT 64)
AND released_at IS NULL AND expires_at>? ORDER BY machine_id,expires_at,lease_id LIMIT 1025) l
JOIN issues i ON i.id=l.issue_id LEFT JOIN workflow_states w ON w.id=i.workflow_state_id`, scope.organization, formatHubTime(now))
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	observed, used, capacity := 0, 0, 0
	for rows.Next() {
		if observed == 1024 {
			return false, nativeLandingCapacityUnknown, nil
		}
		observed++
		var declared int
		var organization tracker.OrganizationID
		var project tracker.ProjectID
		var state string
		if err := rows.Scan(&declared, &organization, &project, &state); err != nil {
			return false, 0, err
		}
		if organization != scope.organization || project != scope.project || declared <= 0 && state != "Merging" {
			continue
		}
		used++
		if declared > 0 && (capacity == 0 || declared < capacity) {
			capacity = declared
		}
	}
	if err := rows.Err(); err != nil {
		return false, 0, err
	}
	if capacity > 0 && used >= capacity {
		return false, capacity, nil
	}
	if used > 0 {
		return false, nativeLandingCapacityUnknown, nil
	}
	return true, 0, nil
}

func readExplanationIssue(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (tracker.NativeIssue, tracker.WorkItemID, error) {
	var issue tracker.NativeIssue
	var id tracker.WorkItemID
	var labels, assignees, actor, contract, created, updated, activity, organizationSlug, projectSlug string
	var priority sql.NullInt64
	args := []any{scope.organization, scope.project, item}
	err := q.QueryRowContext(ctx, `SELECT i.id,i.native_id,i.organization_id,i.project_id,i.number,i.revision,p.profile,i.title,i.body,COALESCE(w.detent_state,''),COALESCE(w.terminal,0),i.archived,i.labels_json,i.assignees_json,i.actor_json,i.issue_contract_json,COALESCE(i.native_created_at,i.created_at),COALESCE(i.native_updated_at,i.created_at),COALESCE(i.last_activity_at,i.created_at),p.require_dependencies=0,o.slug,p.slug,
(SELECT priority_override FROM queue_entries WHERE issue_id=i.id ORDER BY id LIMIT 1)
FROM issues i JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id AND p.organization_id=i.organization_id LEFT JOIN workflow_states w ON w.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, args...).Scan(&id, &issue.WorkItemID, &issue.OrganizationID, &issue.ProjectID, &issue.Number, &issue.Revision, &issue.Profile, &issue.Title, &issue.Body, &issue.State, &issue.Terminal, &issue.Archived, &labels, &assignees, &actor, &contract, &created, &updated, &activity, &issue.IgnoreDependencies, &organizationSlug, &projectSlug, &priority)
	if err != nil {
		return issue, 0, err
	}
	issue.WebURL = issueBrowserPath(organizationSlug, projectSlug, issue.Number)
	if priority.Valid {
		value := int(priority.Int64)
		issue.Priority = &value
	}
	issue.IssueContract = &issuecontract.State{}
	for _, field := range []struct {
		raw   string
		value any
	}{{labels, &issue.Labels}, {assignees, &issue.Assignees}, {actor, &issue.Actor}, {contract, issue.IssueContract}} {
		if err := decodeStorageJSON(ctx, []byte(field.raw), field.value); err != nil {
			return issue, 0, err
		}
	}
	if scope.credential.HostedRole == "reporter" && (scope.actor().PrincipalID == "" || issue.Actor.PrincipalID != scope.actor().PrincipalID) {
		return tracker.NativeIssue{}, 0, sql.ErrNoRows
	}
	var legacy struct {
		Body string `json:"confirmed_body"`
	}
	if err := decodeStorageJSON(ctx, []byte(contract), &legacy); err != nil {
		return issue, 0, err
	}
	if legacy.Body != "" {
		issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(legacy.Body)
	}
	if issue.CreatedAt, err = parseTimeValue(created); err != nil {
		return issue, 0, err
	}
	if issue.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return issue, 0, err
	}
	if issue.LastActivityAt, err = parseTimeValue(activity); err != nil {
		return issue, 0, err
	}
	issue.LinkedSource, err = readLinkedIssueSourceProjection(ctx, q, item, true)
	if err != nil {
		return issue, 0, err
	}
	issue = scope.reporterIssue(issue)
	if err := readExplanationDependencies(ctx, q, scope, id, &issue); err != nil {
		return issue, 0, err
	}
	delivery, found, err := readReleaseDelivery(ctx, q, scope, item)
	if found {
		issue.ReleaseDelivery = &delivery
	}
	return issue, id, err
}

func readExplanationDependencies(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID, issue *tracker.NativeIssue) error {
	grants, args := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	args = append(args, id)
	rows, err := q.QueryContext(ctx, `SELECT i.native_id,i.project_id,i.number,i.title,COALESCE(w.detent_state,''),COALESCE(w.terminal,0),COALESCE(w.dispatchable,0),o.slug,p.slug,(`+grants+`),i.actor_json
FROM (SELECT blocker_issue_id FROM issue_dependencies WHERE dependent_issue_id=? ORDER BY blocker_issue_id LIMIT 101) d
JOIN issues i ON i.id=d.blocker_issue_id JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id LEFT JOIN workflow_states w ON w.id=i.workflow_state_id ORDER BY d.blocker_issue_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	issue.Blockers = []tracker.NativeDependency{}
	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Dependents = []tracker.NativeDependency{}
	issue.DependencyReady = true
	for rows.Next() {
		var dependency tracker.NativeDependency
		var organizationSlug, projectSlug, actor string
		var visible bool
		if err := rows.Scan(&dependency.ID, &dependency.ProjectID, &dependency.Number, &dependency.Title, &dependency.State, &dependency.Terminal, &dependency.Dispatchable, &organizationSlug, &projectSlug, &visible, &actor); err != nil {
			return err
		}
		issue.DependencyReady = issue.DependencyReady && dependency.Terminal
		if scope.credential.HostedRole == "reporter" {
			var owner tracker.Actor
			if err := decodeStorageJSON(ctx, []byte(actor), &owner); err != nil {
				return err
			}
			visible = visible && scope.actor().PrincipalID != "" && owner.PrincipalID == scope.actor().PrincipalID
		}
		if !visible {
			continue
		}
		dependency.Identifier = string(dependency.ProjectID) + "#" + strconv.Itoa(dependency.Number)
		dependency.WebURL = issueBrowserPath(organizationSlug, projectSlug, dependency.Number)
		issue.Blockers = append(issue.Blockers, dependency)
		issue.Dependencies = append(issue.Dependencies, dependency.ID)
	}
	slices.Sort(issue.Dependencies)
	slices.SortFunc(issue.Blockers, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return rows.Err()
}

func boundedNativeExplanation(evidence tracker.NativeRuntimeEvidence, failure *explain.FailureSignature) (explain.IssueExplanation, error) {
	evidence.Issue.IssueContract = nil
	if version := evidence.CurrentVersion; version != nil {
		current := *version
		current.Landing = explanationLanding(version.Landing)
		current.Validation = explanationCommand(version.Validation)
		evidence.CurrentVersion = &current
		if version.Validation != nil && (version.Validation.Pipeline != nil || version.Validation.Evidence != nil) {
			evidence.Unavailable = append(evidence.Unavailable, "current_version_validation_evidence_omitted")
		}
		if version.Landing != nil {
			evidence.Unavailable = append(evidence.Unavailable, "landing_details_omitted")
		}
	}
	result := explain.FromNativeEvidence(evidence)
	result.FailureSignature = failure
	raw, err := json.Marshal(result)
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	if len(raw) < 256*1024 {
		return result, nil
	}
	result.NativeRuntime = &tracker.NativeRuntimeEvidence{
		ChangeSequence: evidence.ChangeSequence,
		Issue:          tracker.NativeIssue{NativeReference: evidence.Issue.NativeReference, State: evidence.Issue.State},
		ObservedAt:     evidence.ObservedAt,
		Unavailable:    []string{"explanation_evidence_truncated"},
	}
	if slices.Contains(evidence.Unavailable, "change_gate_history_truncated") {
		result.NativeRuntime.Unavailable = append(result.NativeRuntime.Unavailable, "change_gate_history_truncated")
	}
	result.Sources = []explain.SourceStatus{{Name: "explanation_evidence_truncated", State: explain.SourceUnavailable}}
	result.Evidence, result.Reasons, result.LatestTransition, result.FailureSignature = nil, nil, nil, nil
	result.Eligibility.Refusals, result.Eligibility.Latest, result.Eligibility.Current = nil, nil, nil
	result.RequiredGate.Reason = operatorfailure.BoundedText(result.RequiredGate.Reason, 4096)
	result.RequiredGate.Failures, result.RequiredGate.Running = nil, nil
	if result.RequiredGate.ExternalMerge != nil {
		result.RequiredGate.ExternalMerge.Detail = operatorfailure.BoundedText(result.RequiredGate.ExternalMerge.Detail, 4096)
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	if len(raw) >= 256*1024 {
		return explain.IssueExplanation{}, errors.New("explanation identity and gate exceed response budget")
	}
	return result, nil
}

func explanationCommand(value *gate.CommandResult) *gate.CommandResult {
	if value == nil {
		return nil
	}
	result := *value
	result.Pipeline, result.Evidence = nil, nil
	return boundedChangeCommand(&result)
}

func explanationLanding(value *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	result := boundedChangeLanding(value)
	if result == nil {
		return nil
	}
	result.Pipeline, result.Packages, result.SecurityAudit = nil, nil, nil
	if result.Gate != nil {
		result.Gate.Evidence, result.Gate.Pipeline = nil, nil
	}
	if result.CI != nil {
		ci := *result.CI
		for _, values := range []*[]string{&ci.RequiredChecks, &ci.PendingChecks, &ci.MissingChecks, &ci.FailedChecks} {
			*values = slices.Clone((*values)[:min(len(*values), 16)])
			for i := range *values {
				(*values)[i] = operatorfailure.BoundedText((*values)[i], 256)
			}
		}
		result.CI = &ci
	}
	return result
}
