package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func policyMismatch(message string) error {
	return &nativeError{Code: "policy_mismatch", Message: message, status: http.StatusConflict}
}

func (s *Service) policyScope(c echo.Context) (string, error) {
	if c.Param("organization") != "" {
		scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project"))}
		var count int
		if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&count); err != nil {
			return "", err
		}
		if count != 1 {
			return "", nativeNotFound()
		}
		return string(scope.organization) + "/" + string(scope.project), nil
	}
	repository := c.Param("owner") + "/" + c.Param("repo")
	var count int
	if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM repositories WHERE github_owner || '/' || github_name = ? COLLATE NOCASE", repository).Scan(&count); err != nil {
		return "", err
	}
	if count != 1 {
		return "", nativeNotFound()
	}
	return "repository:" + strings.ToLower(repository), nil
}

func (s *Service) getProjectPolicy(c echo.Context) error {
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	approval, err := readProjectPolicyWithHistory(c.Request().Context(), s.database.reader, scope, limit, c.QueryParam("after"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, approval)
}

func (s *Service) approveProjectPolicy(c echo.Context) error {
	var change policy.Change
	if err := decodeAPIJSON(c, &change); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := change.Policy.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	approval, err := s.database.approvePolicy(c.Request().Context(), scope, credential.ID, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, approval)
}

func (s *Service) observeProjectPolicy(c echo.Context) error {
	var observation policy.Observation
	if err := decodeAPIJSON(c, &observation); err != nil {
		return invalidAPIRequest(c, err)
	}
	descriptor, err := workflowconfig.ResolveSharedPolicy(observation.Descriptor)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	observation.Descriptor = descriptor
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	reporter := credential.Runner.RunnerID
	if reporter == "" {
		reporter = credential.ID
	}
	if err := storeObservedPolicy(c.Request().Context(), s.database.db, scope, reporter, observation, s.config.now()); err != nil {
		return s.nativeAPIError(c, err)
	}
	if credential.Runner.RunnerID != "" {
		if err := s.database.applyObservedDefaultBranchPolicy(c.Request().Context(), scope, observation); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func storeObservedPolicy(ctx context.Context, exec hostedExecer, scope, reporter string, observation policy.Observation, now time.Time) error {
	encoded, err := json.Marshal(observation.Descriptor)
	if err != nil {
		return err
	}
	source, err := json.Marshal(observation.Source)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO project_observed_policies (scope, policy_id, descriptor_json, runner_id, observed_at, source_json) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(scope, runner_id) DO UPDATE SET policy_id = excluded.policy_id, descriptor_json = excluded.descriptor_json, observed_at = excluded.observed_at,
source_json = CASE WHEN excluded.source_json = 'null' AND project_observed_policies.policy_id = excluded.policy_id THEN project_observed_policies.source_json ELSE excluded.source_json END`, scope, observation.ID, string(encoded), reporter, formatHubTime(now), string(source))
	return err
}

func readObservedPolicies(ctx context.Context, query nativeQueryer, scope string, approved policy.Descriptor, now time.Time) ([]policy.ObservedPolicy, error) {
	rows, err := query.QueryContext(ctx, `WITH current_reports AS (
SELECT o.* FROM project_observed_policies o
LEFT JOIN runner_identities i ON i.id = o.runner_id
JOIN api_tokens t ON t.id = COALESCE(i.token_id, o.runner_id) AND t.revoked_at IS NULL
AND (t.expires_at IS NULL OR julianday(t.expires_at) > julianday(?))
JOIN token_grants g ON g.token_id = t.id AND g.organization_id || '/' || g.project_id = o.scope
WHERE i.removed_at IS NULL
)
SELECT o.descriptor_json, o.source_json, o.runner_id, o.observed_at, o.policy_id,
EXISTS (SELECT 1 FROM policy_revisions r WHERE r.scope = o.scope AND r.policy_id = o.policy_id),
(SELECT count(DISTINCT policy_id) FROM current_reports WHERE scope = o.scope)
FROM current_reports o WHERE o.scope = ? AND o.policy_id <> ? ORDER BY o.observed_at DESC, o.runner_id`, formatHubTime(now), scope, approved.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []policy.ObservedPolicy{}
	seen := map[string]int{}
	for rows.Next() {
		var raw, source, id string
		var observed policy.ObservedPolicy
		var distinct int
		if err := rows.Scan(&raw, &source, &observed.RunnerID, &observed.ObservedAt, &id, &observed.PreviouslyApproved, &distinct); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if index, ok := seen[id]; ok {
			result[index].RunnerIDs = append(result[index].RunnerIDs, observed.RunnerID)
			continue
		}
		seen[id] = len(result)
		if err := json.Unmarshal([]byte(raw), &observed.Policy); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if observed.PreviouslyApproved && observed.Policy.SameAuthoredInputs(approved) && observed.Policy.Authored.Version < approved.Authored.Version {
			delete(seen, id)
			continue
		}
		if err := json.Unmarshal([]byte(source), &observed.Source); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		observed.Conflict = distinct > 1 || observed.PreviouslyApproved
		observed.RunnerIDs = []string{observed.RunnerID}
		result = append(result, observed)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) revokeProjectPolicy(c echo.Context) error {
	var request struct {
		ExpectedID string `json:"expected_policy_id"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	_, err = revokeProjectPolicyInTx(c.Request().Context(), s.database.db, scope, request.ExpectedID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (d *database) approvePolicy(ctx context.Context, scope, actor string, change policy.Change) (result policy.Approval, resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	result, err = d.approvePolicyInTx(ctx, tx, scope, actor, change)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
func (d *database) approvePolicyInTx(ctx context.Context, tx *sql.Tx, scope, actor string, change policy.Change) (result policy.Approval, resultErr error) {
	resolved, err := workflowconfig.ResolveSharedPolicy(change.Policy)
	if err != nil {
		return result, nativeInvalid(err.Error())
	}
	change.Policy = resolved
	if err := validateWorkflowPolicy(ctx, tx, scope, change.Policy); err != nil {
		return result, err
	}
	var current string
	err = tx.QueryRowContext(ctx, "SELECT policy_id FROM project_policies WHERE scope = ?", scope).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if current != change.ExpectedID && current != change.Policy.ID {
		return result, policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
	}
	now, err := d.currentTime()
	if err != nil {
		return result, err
	}
	carryover := false
	if current != "" && current != change.Policy.ID {
		previous, err := readProjectPolicy(ctx, tx, scope)
		if err != nil {
			return result, err
		}
		carryover = change.Policy.SameAuthoredInputs(previous.Policy)
	}
	if current != change.Policy.ID && !carryover {
		organization, project, native := strings.Cut(scope, "/")
		if native && !strings.HasPrefix(scope, "repository:") {
			nativeScope := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}
			var profile, source string
			if err := tx.QueryRowContext(ctx, "SELECT profile, workflow_source FROM projects WHERE organization_id=? AND id=?", organization, project).Scan(&profile, &source); err != nil {
				return result, err
			}
			if profile == "native" {
				if change.Policy.Workflow == nil && source != "" {
					return result, policyMismatch("Repository-controlled workflow requires a resolved workflow in the policy descriptor; inspect the repository definition with an updated runner")
				}
				if change.Policy.Workflow != nil {
					if err := applyNativeProjectStates(ctx, tx, nativeScope, change.Policy.Workflow.States, now); err != nil {
						return result, err
					}
					revision := change.Policy.Workflow.Revision
					if revision == "" {
						revision = change.Policy.SourceRevision
					}
					if _, err := tx.ExecContext(ctx, "UPDATE projects SET workflow_source=?, workflow_source_revision=?, workflow_markdown='', integration_revision=integration_revision+1 WHERE organization_id=? AND id=?", change.Policy.Workflow.Source, revision, organization, project); err != nil {
						return result, err
					}
				}
			}
		}
	}
	if current != change.Policy.ID && (change.Policy.Workflow != nil || carryover) {
		if err := recordWorkflowApply(ctx, tx, scope, actor, current, change.Policy, now); err != nil {
			return result, err
		}
	}
	raw, err := json.Marshal(change.Policy)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING", scope, change.Policy.ID, string(raw), actor, formatHubTime(now)); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO project_policies (scope, policy_id) VALUES (?, ?) ON CONFLICT (scope) DO UPDATE SET policy_id = excluded.policy_id", scope, change.Policy.ID); err != nil {
		return result, err
	}
	if err := followDefaultChangeReviewPolicy(ctx, tx, scope, change.Policy); err != nil {
		return result, err
	}
	result, err = readProjectPolicy(ctx, tx, scope)
	if err != nil {
		return result, err
	}
	return result, nil
}

func defaultChangeReviewPolicy(descriptor policy.Descriptor) tracker.ChangeReviewPolicy {
	rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: descriptor.Gates.HumanReview, RequiredChecks: []tracker.ChangeCheckSpec{}}
	rules.ID = changerequest.PolicyID(rules)
	return rules
}

// followsRepositoryGate reports a review policy that pins no CI check. Its
// review requirement is not a separate decision: it follows the repository
// gate, the way the default does, so a descriptor approval may rewrite it.
func followsRepositoryGate(rules tracker.ChangeReviewPolicy) bool {
	return rules.ID != "" && len(rules.RequiredChecks) == 0
}

// followDefaultChangeReviewPolicy keeps a native project's review policy usable
// across a repository policy approval. A project with no review policy, or
// one that pins no checks, gets the default under the approved descriptor. A
// review policy an administrator shaped by pinning checks is never
// rewritten: it goes stale, and publishing says so until the administrator
// approves it again against the new descriptor. A descriptor whose gates the
// default cannot satisfy leaves the project as it is.
func followDefaultChangeReviewPolicy(ctx context.Context, tx *sql.Tx, scope string, descriptor policy.Descriptor) error {
	organization, project, ok := strings.Cut(scope, "/")
	if !ok || strings.HasPrefix(scope, "repository:") {
		return nil
	}
	native := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}
	current, err := readChangePolicy(ctx, tx, native)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rules := defaultChangeReviewPolicy(descriptor)
	if err == nil && (current.ID == rules.ID || !followsRepositoryGate(current)) {
		return nil
	}
	if changerequest.ValidatePolicy(rules, descriptor) != nil {
		return nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_review_policies (organization_id, project_id, policy_json) VALUES (?, ?, ?)
ON CONFLICT (organization_id, project_id) DO UPDATE SET policy_json = excluded.policy_json`, native.organization, native.project, string(raw))
	return err
}

// backfillChangeReviewPolicies gives every project with an approved
// repository policy the review policy approving it now seeds: projects
// approved before seeding existed had none, so no run could publish a
// version, and projects seeded while the default required review follow the
// repository gate like any other default. It is hub migration 40.
func backfillChangeReviewPolicies(ctx context.Context, tx *sql.Tx) error {
	scopes, err := approvedProjectScopes(ctx, tx)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		approval, err := readProjectPolicy(ctx, tx, scope)
		if err != nil {
			continue
		}
		if err := followDefaultChangeReviewPolicy(ctx, tx, scope, approval.Policy); err != nil {
			return fmt.Errorf("backfill review policy for %s: %w", scope, err)
		}
	}
	return nil
}

func approvedProjectScopes(ctx context.Context, tx *sql.Tx) (scopes []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope FROM project_policies WHERE scope NOT LIKE 'repository:%' ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

type policyQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readProjectPolicy(ctx context.Context, db policyQuerier, scope string) (policy.Approval, error) {
	var result policy.Approval
	var raw string
	err := db.QueryRowContext(ctx, `SELECT r.metadata_json, r.approved_by, r.approved_at
FROM project_policies p JOIN policy_revisions r ON r.scope = p.scope AND r.policy_id = p.policy_id WHERE p.scope = ?`, scope).Scan(&raw, &result.ApprovedBy, &result.ApprovedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, policyMismatch("No approved repository policy; resolve the customer definition and ask an administrator to approve its descriptor")
	}
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(raw), &result.Policy); err != nil {
		return result, err
	}
	if result.Policy.Authored != nil && len(result.Policy.Authored.Files) > 0 {
		result.Policy, err = workflowconfig.ResolveSharedPolicy(result.Policy)
		return result, err
	}
	return result, result.Policy.Validate()
}

func claimPolicyScope(query claimCandidateQuery) (string, error) {
	if query.NativeScope != nil {
		return string(query.NativeScope.organization) + "/" + string(query.NativeScope.project), nil
	}
	if len(query.Repositories) != 1 {
		return "", policyMismatch("A repository policy claim requires exactly one repository")
	}
	return "repository:" + strings.ToLower(strings.TrimSpace(query.Repositories[0])), nil
}

func validateWorkflowPolicy(ctx context.Context, db policyQuerier, scope string, descriptor policy.Descriptor) error {
	organization, project, native := strings.Cut(scope, "/")
	if !native || strings.HasPrefix(scope, "repository:") || descriptor.Workflow != nil {
		return nil
	}
	var profile, markdown string
	if err := db.QueryRowContext(ctx, "SELECT profile, workflow_markdown FROM projects WHERE organization_id=? AND id=?", organization, project).Scan(&profile, &markdown); err != nil {
		return err
	}
	if profile != "native" || markdown == "" {
		return nil
	}
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
	if err != nil {
		return policyMismatch("Saved Cloud workflow is invalid; correct its Markdown definition before approving execution")
	}
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
		expected, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			return err
		}
		if descriptor.Authored.Files["WORKFLOW.md"] != expected.Authored.Files["WORKFLOW.md"] {
			return policyMismatch("Cloud workflow changed; load its current Markdown definition and approve the resolved policy before claiming work")
		}
	} else if descriptor.Configuration == nil || descriptor.Configuration.DefinitionDigest != workflow.SourceHash {
		return policyMismatch("Cloud workflow changed; load its current Markdown definition and approve the resolved policy before claiming work")
	}
	return nil
}

func validateClaimPolicy(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, machine tracker.MachineID) (string, error) {
	scope, err := claimPolicyScope(query)
	if err != nil {
		return "", err
	}
	approval, err := readProjectPolicy(ctx, tx, scope)
	if err != nil {
		return "", err
	}
	matches, err := approvedPolicyRevisionMatches(ctx, tx, scope, query.PolicyID, approval.Policy)
	if err != nil {
		return "", err
	}
	if !matches {
		return "", policyMismatch("Runner policy is missing or stale; load the approved repository definition and permitted local overrides before claiming work")
	}
	if err := validateWorkflowPolicy(ctx, tx, scope, approval.Policy); err != nil {
		return "", err
	}
	var runnerID string
	if query.NativeScope != nil {
		runnerID = query.NativeScope.credential.Runner.RunnerID
	}
	var tags []string
	if runnerID == "" && (approval.Policy.Requirements.RunnerID != "" || approval.Policy.Requirements.MachineID != "" || len(approval.Policy.Requirements.RequiredTags) != 0) {
		return "", &nativeError{Code: "selector_no_match", Message: "Constrained routing requires an administrator-enrolled runner; legacy registration cannot assert trusted host identity or tags", status: http.StatusConflict}
	}
	if runnerID != "" {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT tags_json FROM runner_identities WHERE id = ? AND machine_id = ? AND organization_id = ?", runnerID, machine, query.NativeScope.organization).Scan(&raw); err != nil {
			return "", err
		}
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			return "", err
		}
	}
	if err := approval.Policy.Requirements.Match(runnerID, string(machine), tags); err != nil {
		return "", &nativeError{Code: "selector_no_match", Message: err.Error(), status: http.StatusConflict}
	}
	return scope, nil
}

func (d *database) leasePolicyID(ctx context.Context, lease tracker.LeaseID) (string, error) {
	var id string
	err := d.db.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ?", lease).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func requireApprovedLeasePolicy(ctx context.Context, tx *sql.Tx, lease tracker.LeaseID, required bool) error {
	var pinned, approved string
	var recorded bool
	err := tx.QueryRowContext(ctx, `SELECT l.policy_id, coalesce(p.policy_id, ''),
EXISTS (SELECT 1 FROM policy_revisions r WHERE r.scope = l.scope AND r.policy_id = l.policy_id)
FROM lease_policies l LEFT JOIN project_policies p ON p.scope = l.scope WHERE l.lease_id = ?`, lease).Scan(&pinned, &approved, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		if required {
			return policyMismatch("Legacy lease has no pinned policy; release it and request a new approved claim")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if pinned == "" || approved == "" || !recorded {
		return policyMismatch("Pinned policy is missing or project approval has been revoked; stop the attempt and obtain administrator approval before restarting")
	}
	return requireLeaseRouting(ctx, tx, lease)
}

func approvedPolicyRevisionMatches(ctx context.Context, query policyQuerier, scope, id string, approved policy.Descriptor) (bool, error) {
	if id == "" {
		return false, nil
	}
	if id == approved.ID {
		return true, nil
	}
	var raw string
	if err := query.QueryRowContext(ctx, "SELECT metadata_json FROM policy_revisions WHERE scope=? AND policy_id=?", scope, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var previous policy.Descriptor
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return false, err
	}
	return previous.Match(approved) == nil, nil
}

func requireLeaseRouting(ctx context.Context, tx *sql.Tx, lease tracker.LeaseID) error {
	var runner, machine, tags, state, raw string
	var access bool
	err := tx.QueryRowContext(ctx, `SELECT r.id, r.machine_id, r.tags_json, r.state, p.metadata_json,
EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id = r.token_id AND g.organization_id = i.organization_id AND g.project_id = i.project_id)
FROM lease_runners lr JOIN runner_identities r ON r.id = lr.runner_id JOIN leases l ON l.lease_id = lr.lease_id
JOIN issues i ON i.id = l.issue_id JOIN lease_policies lp ON lp.lease_id = l.lease_id
JOIN policy_revisions p ON p.scope = lp.scope AND p.policy_id = lp.policy_id WHERE lr.lease_id = ?`, lease).Scan(&runner, &machine, &tags, &state, &raw, &access)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !access {
		return nativeNotFound()
	}
	if state == "disabled" {
		return &nativeError{Code: "runner_disabled", Message: "Runner is disabled", status: http.StatusConflict}
	}
	var descriptor policy.Descriptor
	var authorizedTags []string
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(tags), &authorizedTags); err != nil {
		return err
	}
	if err := descriptor.Requirements.Match(runner, machine, authorizedTags); err != nil {
		return &nativeError{Code: "selector_no_match", Message: err.Error(), status: http.StatusConflict}
	}
	return nil
}

func revokeProjectPolicyInTx(ctx context.Context, exec hostedExecer, scope, expectedID string) (any, error) {
	result, err := exec.ExecContext(ctx, "DELETE FROM project_policies WHERE scope = ? AND policy_id = ?", scope, expectedID)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, policyMismatch("Policy changed or is already revoked; inspect the current approval before revoking it")
	}
	return struct {
		Status string `json:"status"`
	}{"revoked"}, nil
}
