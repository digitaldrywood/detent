package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type CutoverRequest struct {
	ClosedState string `json:"closed_state"`
	tracker.Mutation
	DryRun         bool                  `json:"dry_run"`
	Checkpoint     string                `json:"checkpoint"`
	AcceptPartial  bool                  `json:"accept_partial"`
	CloseSource    bool                  `json:"close_source"`
	DestinationURL string                `json:"destination_url"`
	States         []tracker.NativeState `json:"states"`
	InitialState   string                `json:"initial_state"`
}

type CutoverReceipt struct {
	Checkpoint             string             `json:"checkpoint"`
	Applied                bool               `json:"applied"`
	Issues                 int                `json:"issues"`
	IncompleteImports      int                `json:"incomplete_imports"`
	UnresolvedDependencies int                `json:"unresolved_dependencies"`
	Blockers               []string           `json:"blockers"`
	HistoryLimitations     []string           `json:"history_limitations"`
	CloseSource            bool               `json:"close_source"`
	Integration            ProjectIntegration `json:"integration"`
}

func (s *Service) cutoverProject(c echo.Context) error {
	var request CutoverRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if err := validateNativeStates(request.States); err != nil {
			return nil, err
		}
		initialFound := false
		for _, state := range request.States {
			if state.Name == request.InitialState {
				initialFound = true
			}
		}
		if !initialFound {
			return nil, nativeInvalid("Initial state must be a configured native workflow state")
		}
		if request.CloseSource {
			destination, err := url.Parse(request.DestinationURL)
			if err != nil || destination.Host == "" || destination.User != nil || (destination.Scheme != "https" && destination.Scheme != "http") {
				return nil, nativeInvalid("Link-and-close requires an absolute Detent destination URL")
			}
		}
		receipt, err := inspectCutover(ctx, tx, scope, request, now)
		if err != nil {
			return nil, err
		}
		if request.DryRun {
			return receipt, nil
		}
		if len(receipt.Blockers) != 0 {
			return nil, nativeInvalid(strings.Join(receipt.Blockers, "; "))
		}
		if request.Checkpoint == "" || receipt.Checkpoint != request.Checkpoint {
			return nil, nativeInvalid("Cutover checkpoint changed; repeat the dry run and review its receipt")
		}
		if request.CloseSource && s.config.OutboxBackend == nil {
			return nil, nativeInvalid("Link-and-close requires configured GitHub transport")
		}
		if err := applyCutover(ctx, tx, scope, request, now); err != nil {
			return nil, err
		}
		receipt.Applied = true
		receipt.Integration, err = readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		raw, err := marshalNative(receipt)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO github_cutovers (project_id, checkpoint, receipt_json, actor_id, created_at) VALUES (?, ?, ?, ?, ?)", scope.project, receipt.Checkpoint, raw, scope.credential.ID, formatHubTime(now))
		return receipt, err
	})
}

func inspectCutover(ctx context.Context, tx *sql.Tx, scope nativeScope, request CutoverRequest, now time.Time) (CutoverReceipt, error) {
	result := CutoverReceipt{Blockers: []string{}, HistoryLimitations: []string{}, CloseSource: request.CloseSource}
	integration, err := readProjectIntegration(ctx, tx, scope)
	if err != nil {
		return result, err
	}
	result.Integration = integration
	if integration.Profile != "github_compatible" {
		result.Blockers = append(result.Blockers, "Project is already native; automatic rollback is not supported")
	}
	if err := requireIntegrationIdle(ctx, tx, scope, now); err != nil {
		result.Blockers = append(result.Blockers, err.Error())
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.native_id, i.revision, i.source_updated_at, COALESCE(g.revision, 0), COALESCE(g.status, 'missing'), COALESCE(g.source_updated_at, ''), COALESCE(g.gaps_json, '[]'), COALESCE(ws.detent_state, '')
FROM issues i LEFT JOIN github_imports g ON g.work_item_id = i.native_id LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.project_id = ? ORDER BY i.native_id`, scope.project)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	fingerprint := []any{integration, request.States, request.InitialState, request.ClosedState, request.AcceptPartial, request.CloseSource, request.DestinationURL}
	for rows.Next() {
		var id, source, status, imported, gaps, state string
		var revision, importRevision int64
		if err := rows.Scan(&id, &revision, &source, &importRevision, &status, &imported, &gaps, &state); err != nil {
			return result, err
		}
		result.Issues++
		fingerprint = append(fingerprint, []any{id, revision, source, importRevision, status, imported, gaps, state})
		if status != "retrieved" || !sameImportSourceTime(source, imported) {
			result.IncompleteImports++
		}
		if status == "missing" {
			result.Blockers = append(result.Blockers, "Import source issue "+id+" before cutover")
		}
		var limitations []string
		if err := json.Unmarshal([]byte(gaps), &limitations); err != nil {
			return result, err
		}
		result.HistoryLimitations = append(result.HistoryLimitations, limitations...)
		if state != "" {
			found := false
			for _, configured := range request.States {
				if configured.Name == state {
					found = true
				}
			}
			if !found {
				result.Blockers = append(result.Blockers, "Native states must preserve existing state "+state)
			}
		}
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if result.IncompleteImports != 0 && !request.AcceptPartial {
		result.Blockers = append(result.Blockers, "Finish all import pages and reimport changed source issues, or explicitly accept partial history")
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM github_import_records r JOIN github_imports g ON g.id = r.import_id
WHERE g.project_id = ? AND r.kind = 'dependency' AND NOT EXISTS (SELECT 1 FROM issues i WHERE i.project_id = g.project_id AND i.github_node_id = json_extract(r.record_json, '$.dependency_id'))`, scope.project).Scan(&result.UnresolvedDependencies)
	if err != nil {
		return result, err
	}
	if result.UnresolvedDependencies != 0 {
		result.Blockers = append(result.Blockers, "Import unresolved dependency issues into this project before cutover")
	}
	var closed int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM issues WHERE project_id = ? AND github_state <> 'open'", scope.project).Scan(&closed); err != nil {
		return result, err
	}
	closedStateValid := false
	for _, state := range request.States {
		if state.Name == request.ClosedState && state.Terminal {
			closedStateValid = true
		}
	}
	if closed > 0 && !closedStateValid {
		result.Blockers = append(result.Blockers, "Specify a terminal closed_state to preserve closed GitHub issues")
	}
	var sourceVersions, policies string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(group_concat(source_version, ','), '') FROM (SELECT source_version FROM issues WHERE project_id = ? ORDER BY native_id)", scope.project).Scan(&sourceVersions); err != nil {
		return result, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(group_concat(policy_id, ','), '') FROM (SELECT policy_id FROM project_policies WHERE scope IN (?, ?) ORDER BY scope)", "repository:"+strings.ToLower(integration.Repository), string(scope.organization)+"/"+string(scope.project)).Scan(&policies); err != nil {
		return result, err
	}
	fingerprint = append(fingerprint, sourceVersions, policies)
	fingerprint = append(fingerprint, result.UnresolvedDependencies)
	raw, err := json.Marshal(fingerprint)
	if err != nil {
		return result, err
	}
	digest := sha256.Sum256(raw)
	result.Checkpoint = hex.EncodeToString(digest[:])
	return result, nil
}

func sameImportSourceTime(left, right string) bool {
	a, err := time.Parse(time.RFC3339Nano, left)
	if err != nil {
		return false
	}
	b, err := time.Parse(time.RFC3339Nano, right)
	return err == nil && a.Equal(b)
}

func applyCutover(ctx context.Context, tx *sql.Tx, scope nativeScope, request CutoverRequest, now time.Time) error {
	stamp := formatHubTime(now)
	for _, state := range request.States {
		_, err := tx.ExecContext(ctx, "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", scope.project, state.Name, state.Name, state.Terminal, state.Dispatchable, stamp, stamp)
		if err != nil {
			return err
		}
	}
	states, err := marshalNative(request.States)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE projects SET profile = 'native', states_json = ?, github_intake = 'disabled', integration_revision = integration_revision + 1 WHERE id = ?", states, scope.project)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE issues SET revision = revision + 1, native_updated_at = ?, workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = CASE WHEN issues.github_state <> 'open' THEN ? ELSE COALESCE((SELECT detent_state FROM workflow_states WHERE id = issues.workflow_state_id), ?) END) WHERE project_id = ?`, stamp, scope.project, request.ClosedState, request.InitialState, scope.project)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at)
SELECT b.id, i.id, 'github_import', ?, ? FROM github_import_records r JOIN github_imports g ON g.id = r.import_id JOIN issues i ON i.native_id = g.work_item_id JOIN issues b ON b.project_id = i.project_id AND b.github_node_id = json_extract(r.record_json, '$.dependency_id') WHERE g.project_id = ? AND r.kind = 'dependency' ON CONFLICT DO NOTHING`, stamp, stamp, scope.project)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE queue_entries SET scope = ?, workflow_state_id = (SELECT workflow_state_id FROM issues WHERE id = queue_entries.issue_id), state = (SELECT ws.detent_state FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.id = queue_entries.issue_id), updated_at = ? WHERE issue_id IN (SELECT id FROM issues WHERE project_id = ?)`, scope.project, stamp, scope.project)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, created_at, updated_at)
SELECT i.id, i.workflow_state_id, i.project_id, ws.detent_state, i.native_id, ?, ? FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.project_id = ? AND NOT EXISTS (SELECT 1 FROM queue_entries q WHERE q.issue_id = i.id)`, stamp, stamp, scope.project)
	if err != nil {
		return err
	}
	if err := supersedeDisallowedOutbox(ctx, tx, scope, now); err != nil {
		return err
	}
	if err := preserveCutoverPolicy(ctx, tx, scope); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT native_id FROM issues WHERE project_id = ? ORDER BY native_id", scope.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		issue, _, err := readNativeIssue(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := recordNativeChange(ctx, tx, scope, issue, id, issue.Revision, "issue.cutover", tracker.CollaborationData{Revision: issue.Revision}, now); err != nil {
			return err
		}
		if request.CloseSource {
			body := fmt.Sprintf("This issue is now managed in Detent: %s\n\nNative work item: %s", request.DestinationURL, id)
			if err := enqueueNativeSummary(ctx, tx, scope, id, request.Checkpoint+":"+id, body, true, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func preserveCutoverPolicy(ctx context.Context, tx *sql.Tx, scope nativeScope) error {
	integration, err := readProjectIntegration(ctx, tx, scope)
	if err != nil {
		return err
	}
	legacy := "repository:" + strings.ToLower(integration.Repository)
	native := string(scope.organization) + "/" + string(scope.project)
	var conflicts int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM project_policies a JOIN project_policies b ON a.policy_id <> b.policy_id WHERE a.scope = ? AND b.scope = ?", legacy, native).Scan(&conflicts); err != nil {
		return err
	}
	if conflicts != 0 {
		return nativeInvalid("Native and repository policy approvals differ; reconcile trusted approvals before cutover")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at) SELECT ?, r.policy_id, r.metadata_json, r.approved_by, r.approved_at FROM policy_revisions r JOIN project_policies p ON p.scope = r.scope AND p.policy_id = r.policy_id WHERE p.scope = ? ON CONFLICT DO NOTHING`, native, legacy)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO project_policies (scope, policy_id) SELECT ?, policy_id FROM project_policies WHERE scope = ? ON CONFLICT DO NOTHING", native, legacy)
	return err
}

func (s *Service) projectNativeSummary(c echo.Context) error {
	var request struct {
		tracker.Mutation
		Body string `json:"body"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		integration, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if integration.Profile != "native" || integration.Projection != "summary" {
			return nil, nativeInvalid("Native summary projection is not enabled")
		}
		if err := enqueueNativeSummary(ctx, tx, scope, c.Param("item"), request.IdempotencyKey, request.Body, false, now); err != nil {
			return nil, err
		}
		return struct {
			Status string `json:"status"`
		}{"pending"}, nil
	})
}

func enqueueNativeSummary(ctx context.Context, tx *sql.Tx, scope nativeScope, item, key, body string, closeSource bool, now time.Time) error {
	if strings.TrimSpace(body) == "" || len(body) > 60<<10 {
		return nativeInvalid("Summary must contain 1 byte to 60 KiB")
	}
	var issueID, repositoryID int64
	if err := tx.QueryRowContext(ctx, "SELECT id, repository_id FROM issues WHERE project_id = ? AND native_id = ? AND github_node_id IS NOT NULL", scope.project, item).Scan(&issueID, &repositoryID); err != nil {
		return err
	}
	desired, err := marshalNative(WorkpadDesired{Phase: "summary", Body: body, Marker: "<!-- detent-summary:" + item + " -->", Summary: true, CloseSource: closeSource})
	if err != nil {
		return err
	}
	coalesce := "summary:" + item
	if closeSource {
		coalesce = "cutover:" + item
	}
	_, err = tx.ExecContext(ctx, "UPDATE github_outbox SET status = 'superseded', completed_at = ?, updated_at = ? WHERE coalesce_key = ? AND status IN ('pending', 'retrying')", formatOutboxTime(now), formatOutboxTime(now), coalesce)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO github_outbox (idempotency_key, repository_id, issue_id, mutation_kind, desired_json, status, coalesce_key, created_at, updated_at) VALUES (?, ?, ?, 'workpad', ?, 'pending', ?, ?, ?)`, "native:"+item+":"+key, repositoryID, issueID, desired, coalesce, formatOutboxTime(now), formatOutboxTime(now))
	return err
}
