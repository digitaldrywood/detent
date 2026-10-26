package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errNoApprovedRepositoryPolicy = &nativeError{Code: "policy_not_approved", Message: "No approved repository policy; resolve the customer definition and ask an administrator to approve its descriptor", status: http.StatusConflict}

func policyMismatch(message string) error {
	return &nativeError{Code: "policy_mismatch", Message: message, status: http.StatusConflict}
}

func (s *Service) policyScope(c echo.Context) (string, error) {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if c.Param("organization") != "" {
		scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project"))}
		var count int
		if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&count); err != nil {
			return "", err
		}
		if count != 1 {
			return "", nativeNotFound()
		}
		return string(scope.organization) + "/" + string(scope.project), nil
	}
	repository := c.Param("owner") + "/" + c.Param("repo")
	var count int
	if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM repositories WHERE github_owner || '/' || github_name = ? COLLATE NOCASE", repository).Scan(&count); err != nil {
		return "", err
	}
	if count != 1 {
		return "", nativeNotFound()
	}
	return "repository:" + strings.ToLower(repository), nil
}

func (s *Service) getProjectPolicy(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	var approval policy.Approval
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if ok && credential.Runner.Valid() {
		approval, _, err = readProjectPolicy(ctx, s.database.reader, scope)
	} else {
		approval, err = readProjectPolicyWithHistory(ctx, s.database.reader, scope, limit, c.QueryParam("after"))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	view, err := s.projectPolicyRead(ctx, s.database.reader, scope, approval, !ok || !credential.Runner.Valid())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) approveProjectPolicy(c echo.Context) error {
	var change policy.Change
	if err := decodeAPIJSON(c, &change); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if change.ObservedPolicyID == "" {
		if err := change.Policy.Validate(); err != nil {
			return s.nativeAPIError(c, nativeInvalid(err.Error()))
		}
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
		return s.invalidAPIRequest(c, err)
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
	return storeEncodedObservedPolicy(ctx, exec, scope, reporter, observation.Descriptor, string(encoded), string(source), now)
}

func storeEncodedObservedPolicy(ctx context.Context, exec hostedExecer, scope, reporter string, descriptor policy.Descriptor, encoded, source string, now time.Time) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO project_observed_policies (scope, policy_id, descriptor_json, runner_id, observed_at, source_json) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(scope, runner_id) DO UPDATE SET policy_id = excluded.policy_id, descriptor_json = excluded.descriptor_json, observed_at = excluded.observed_at,
source_json = CASE WHEN excluded.source_json = 'null' AND project_observed_policies.policy_id = excluded.policy_id THEN project_observed_policies.source_json ELSE excluded.source_json END`, scope, descriptor.ID, encoded, reporter, formatHubTime(now), source)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at, source_digest, workflow_revision) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING", scope, descriptor.ID, encoded, "runner-report", formatHubTime(now), descriptor.SourceDigest, policyWorkflowRevision(descriptor))
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
EXISTS (SELECT 1 FROM policy_revisions r WHERE r.scope = o.scope AND r.policy_id = o.policy_id AND r.approved_by <> 'runner-report'),
(SELECT count(DISTINCT policy_id) FROM current_reports WHERE scope = o.scope),
EXISTS (SELECT 1 FROM policy_revisions r WHERE r.scope = o.scope AND r.policy_id = ? AND (r.approved_by = 'repository-default-branch' OR EXISTS (SELECT 1 FROM project_workflow_applies a WHERE a.scope = r.scope AND a.definition_digest = json_extract(r.metadata_json, '$.source_digest') AND a.source_commit = json_extract(r.metadata_json, '$.workflow.revision') AND (a.change_id <> '' OR a.pull_request_url <> ''))))
FROM current_reports o WHERE o.scope = ? AND o.policy_id <> ? ORDER BY o.observed_at DESC, o.runner_id`, formatHubTime(now), approved.ID, scope, approved.ID)
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
		var repositoryApproved bool
		if err := rows.Scan(&raw, &source, &observed.RunnerID, &observed.ObservedAt, &id, &observed.PreviouslyApproved, &distinct, &repositoryApproved); err != nil {
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
		if observed.PreviouslyApproved && (repositoryApproved && approved.Workflow != nil || observed.Policy.SameAuthoredInputs(approved) && observed.Policy.Authored.Version < approved.Authored.Version) {
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
		return s.invalidAPIRequest(c, err)
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	_, err = s.database.revokePolicy(c.Request().Context(), scope, credential.ID, request.ExpectedID)
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
	result, err = d.approvePolicyInTx(ctx, tx, scope, actor, change, policyLanding{})
	if err != nil {
		return result, err
	}
	return result, d.commit(ctx, tx)
}
func (d *database) approvePolicyInTx(ctx context.Context, tx *sql.Tx, scope, actor string, change policy.Change, landing policyLanding) (result policy.Approval, resultErr error) {
	if change.ObservedPolicyID != "" {
		if !reflect.ValueOf(change.Policy).IsZero() {
			return result, nativePublicInvalid("Supply either observed_policy_id or policy, not both")
		}
		now, err := d.currentTime()
		if err != nil {
			return result, err
		}
		observed, err := readObservedPolicies(ctx, tx, scope, policy.Descriptor{}, now)
		if err != nil {
			return result, err
		}
		for _, candidate := range observed {
			if candidate.Policy.ID == change.ObservedPolicyID {
				change.Policy = candidate.Policy
				break
			}
		}
		if change.Policy.ID == "" {
			return result, nativePublicInvalid("observed_policy_id does not identify a current authorized observation in this project")
		}
	}
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
		previous, err := requireProjectPolicy(ctx, tx, scope)
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
		if err := recordWorkflowApply(ctx, tx, scope, actor, current, change.Policy, now, landing); err != nil {
			return result, err
		}
	}
	raw, err := json.Marshal(change.Policy)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at, source_digest, workflow_revision) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (scope, policy_id) DO UPDATE SET approved_by = excluded.approved_by, approved_at = excluded.approved_at WHERE policy_revisions.approved_by = 'runner-report' OR ?", scope, change.Policy.ID, string(raw), actor, formatHubTime(now), change.Policy.SourceDigest, policyWorkflowRevision(change.Policy), current != change.Policy.ID); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO project_policies (scope, policy_id) VALUES (?, ?) ON CONFLICT (scope) DO UPDATE SET policy_id = excluded.policy_id WHERE project_policies.policy_id IS NOT excluded.policy_id", scope, change.Policy.ID); err != nil {
		return result, err
	}
	reviewChanged, err := followDefaultChangeReviewPolicy(ctx, tx, scope, change.Policy)
	if err != nil {
		return result, err
	}
	if current != change.Policy.ID || reviewChanged {
		if err := resetNativeBoardPolicy(ctx, tx, scope); err != nil {
			return result, err
		}
	}
	result, err = requireProjectPolicy(ctx, tx, scope)
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
func followDefaultChangeReviewPolicy(ctx context.Context, tx *sql.Tx, scope string, descriptor policy.Descriptor) (bool, error) {
	organization, project, ok := strings.Cut(scope, "/")
	if !ok || strings.HasPrefix(scope, "repository:") {
		return false, nil
	}
	native := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}
	current, err := readChangePolicy(ctx, tx, native)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	rules := defaultChangeReviewPolicy(descriptor)
	if err == nil && (current.ID == rules.ID || !followsRepositoryGate(current)) {
		return false, nil
	}
	if changerequest.ValidatePolicy(rules, descriptor) != nil {
		return false, nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_review_policies (organization_id, project_id, policy_json) VALUES (?, ?, ?)
ON CONFLICT (organization_id, project_id) DO UPDATE SET policy_json = excluded.policy_json`, native.organization, native.project, string(raw))
	return err == nil, err
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
		approval, err := requireProjectPolicy(ctx, tx, scope)
		if err != nil {
			continue
		}
		if _, err := followDefaultChangeReviewPolicy(ctx, tx, scope, approval.Policy); err != nil {
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

func readProjectPolicy(ctx context.Context, db policyQuerier, scope string) (policy.Approval, bool, error) {
	var result policy.Approval
	var raw string
	err := db.QueryRowContext(ctx, `SELECT r.metadata_json, r.approved_by, r.approved_at
FROM project_policies p JOIN policy_revisions r ON r.scope = p.scope AND r.policy_id = p.policy_id WHERE p.scope = ?`, scope).Scan(&raw, &result.ApprovedBy, &result.ApprovedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	result, err = decodeProjectPolicy(raw, result)
	return result, err == nil, err
}

func requireProjectPolicy(ctx context.Context, db policyQuerier, scope string) (policy.Approval, error) {
	approval, found, err := readProjectPolicy(ctx, db, scope)
	if err != nil {
		return approval, err
	}
	if !found {
		return approval, errNoApprovedRepositoryPolicy
	}
	return approval, nil
}

type projectPolicyRead struct {
	policy.Approval
	Policy                       *policy.Descriptor      `json:"policy"`
	ObservedPolicies             []policy.ObservedPolicy `json:"observed_policies,omitempty"`
	ObservedPolicyDetailsOmitted bool                    `json:"observed_policy_details_omitted,omitempty"`
	ObservedPoliciesOmitted      int                     `json:"observed_policies_omitted,omitempty"`
}

func (s *Service) projectPolicyRead(ctx context.Context, query nativeQueryer, scope string, approval policy.Approval, observations bool) (projectPolicyRead, error) {
	result := projectPolicyRead{Approval: approval}
	if approval.Policy.ID != "" {
		result.Policy = &result.Approval.Policy
	}
	if observations {
		var err error
		result.ObservedPolicies, err = readObservedPolicies(ctx, query, scope, approval.Policy, s.config.now())
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func decodeProjectPolicy(raw string, result policy.Approval) (policy.Approval, error) {
	var err error
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
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT metadata_json FROM policy_revisions WHERE scope = ? AND policy_id = ?", scope, query.PolicyID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nativePublicInvalid("Claim configuration has not been reported for this project")
		}
		return "", err
	}
	approval, err := decodeProjectPolicy(raw, policy.Approval{})
	if err != nil {
		return "", err
	}
	var runnerID string
	if query.NativeScope != nil {
		runnerID = query.NativeScope.credential.Runner.RunnerID
	}
	var tags []string
	if runnerID == "" && (approval.Policy.Requirements.RunnerID != "" || approval.Policy.Requirements.MachineID != "" || len(approval.Policy.Requirements.RequiredTags) != 0) {
		return "", &nativeError{Code: "selector_no_match", Message: "Constrained routing requires an administrator-enrolled runner; legacy registration cannot assert trusted host identity or tags", status: http.StatusConflict, publicMessage: true}
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var id string
	err := d.reader.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ?", lease).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func requireLeaseRouting(ctx context.Context, tx nativeQueryer, lease tracker.LeaseID) error {
	var runner, machine, tags, state, raw string
	var access bool
	err := tx.QueryRowContext(ctx, `SELECT r.id, r.machine_id, r.tags_json, r.state, p.metadata_json,
`+runnerProjectAccessSQL("i.organization_id", "i.project_id")+`
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
		return nativeNotFoundAt("lease_routing_grant")
	}
	if state == "disabled" {
		return &nativeError{Code: "runner_disabled", Message: "Runner is disabled", status: http.StatusConflict, publicMessage: true}
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

func (d *database) revokePolicy(ctx context.Context, scope, actor, expectedID string) (result any, resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	now, err := d.currentTime()
	if err != nil {
		return nil, err
	}
	result, err = revokeProjectPolicyInTx(ctx, tx, scope, actor, expectedID, now)
	if err != nil {
		return nil, err
	}
	return result, d.commit(ctx, tx)
}

func revokeProjectPolicyInTx(ctx context.Context, tx *sql.Tx, scope, actor, expectedID string, now time.Time) (any, error) {
	return nil, &nativeError{Code: "operation_retired", Message: "Repository configuration does not require execution approval; pause the runner or remove project access to stop work", status: http.StatusGone, publicMessage: true}
}
