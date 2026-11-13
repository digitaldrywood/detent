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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type explanationReadKey struct{}

func explanationRead(ctx context.Context) bool {
	value, ok := ctx.Value(explanationReadKey{}).(bool)
	return ok && value
}

func (s *Service) readExplanationCurrentChange(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, now time.Time) (*tracker.ChangeDetail, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, nativeRunEventStorageUnavailable()
	}
	var change eventstore.Change
	var version *eventstore.Version
	var policy currentChangePolicyRows
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeBulkExportItem(ctx, r, scope, item); err != nil {
			return err
		}
		row, err := r.LatestChange(ctx, scope.project, item)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project {
			return corestore.ErrNotFound
		}
		change = row
		if change.CurrentVersionID != nil {
			current, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			version = &current
		}
		policy, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if change.ID == "" {
		return nil, nil
	}
	detail, err := s.hydrateCurrentChangeDetail(ctx, scope, item, change, version, policy, now)
	if err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeBulkExportItem(ctx, r, scope, item)
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	return &detail, nil
}

func attachRuntimeChangeDetail(result *tracker.NativeRuntimeEvidence, detail tracker.ChangeDetail) {
	if len(detail.Versions) != 0 {
		version := detail.Versions[0]
		result.CurrentVersion = &tracker.NativeChangeVersionEvidence{NativeChangeReference: tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}, PublishedAt: version.CreatedAt, Validation: boundedChangeCommand(version.Validation), Landing: boundedChangeLanding(version.Landing)}
	}
	if detail.Summary.Status == "evidence_incomplete" {
		result.Unavailable = append(result.Unavailable, "change_gate_history_truncated")
	}
	detail.Change.Body = ""
	detail.Change.LinkedIssues = nil
	detail.Versions, detail.Reviews, detail.Checks, detail.Discussion = nil, nil, nil, nil
	detail.Summary.Messages = nil
	result.Change = &detail
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
		if err := rows.Scan(&f.AttemptID, &status, &started, &updated, &raw, &signature); err != nil {
			return nil, false, err
		}
		var data tracker.NativeRunData
		if err := decodeStorageJSON(ctx, []byte(raw), &data); err != nil {
			return nil, false, err
		}
		f.Example, f.Class = failureFromAttempt(data, status)
		if signature != "" {
			f.Example = signature
		}
		if f.Example == "" {
			continue
		}
		f.WorkItem = item
		f.Started, err = parseTimeValue(started)
		if err != nil {
			return nil, false, err
		}
		f.Finished, err = parseTimeValue(updated)
		if err != nil {
			return nil, false, err
		}
		f.At = f.Finished
		if data.Runtime != nil && data.Runtime.Landing != nil && !data.Runtime.Landing.ObservedAt.IsZero() {
			f.At = data.Runtime.Landing.ObservedAt
		}
		failures = append(failures, f)
	}
	return failures, false, rows.Err()
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
	result.RequiredGate.Reason = boundedFailureText(result.RequiredGate.Reason, 4096)
	result.RequiredGate.Failures, result.RequiredGate.Running = nil, nil
	if result.RequiredGate.ExternalMerge != nil {
		result.RequiredGate.ExternalMerge.Detail = boundedFailureText(result.RequiredGate.ExternalMerge.Detail, 4096)
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
				(*values)[i] = boundedFailureText((*values)[i], 256)
			}
		}
		result.CI = &ci
	}
	return result
}
