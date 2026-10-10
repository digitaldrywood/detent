package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coordinatorRunnerTools() []runner.AgentTool {
	return []runner.AgentTool{
		coordinatorTool("get_runners", "Read runners assigned to this project, including selected isolation tier, backend tier support and current problems. Supply runner_id for one runner; otherwise returns a bounded page.", `{"type":"object","properties":{"runner_id":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
		coordinatorTool("set_runner_tier", "Preview changing a runner's access tier without re-enrollment. Organization owner/admin only. The user must approve this preview even when ordinary chat confirmation is disabled.", `{"type":"object","required":["runner_id","isolation_tier"],"properties":{"runner_id":{"type":"string"},"isolation_tier":{"type":"string","enum":["sandbox","native-trusted"]}},"additionalProperties":false}`),
	}
}

type coordinatorRunnerArguments struct {
	RunnerID      string `json:"runner_id"`
	IsolationTier string `json:"isolation_tier,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Offset        int    `json:"offset,omitempty"`
}

type coordinatorRunnerChange struct {
	RunnerID         string `json:"runner_id"`
	IsolationTier    string `json:"isolation_tier"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (s *Service) coordinatorRunner(ctx context.Context, scope nativeScope, id string) (runnerauth.Runner, error) {
	var visible bool
	err := s.database.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runner_identities r WHERE r.organization_id=? AND r.id=? AND r.removed_at IS NULL AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.organization_id=r.organization_id AND g.project_id=?)))`, scope.organization, id, scope.project).Scan(&visible)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if !visible {
		return runnerauth.Runner{}, nativeNotFound()
	}
	return readRunnerWithClock(ctx, s.database.db, scope.organization, id, s.config.now)
}

func (t *coordinatorToolset) runnerTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var args coordinatorRunnerArguments
	if decodeCoordinatorArguments(call.Arguments, &args) != nil || args.Limit < 0 || args.Limit > 50 || args.Offset < 0 {
		return nil, operatortool.ErrInvalidArguments
	}
	manage := call.Name == "set_runner_tier"
	if manage && (args.RunnerID == "" || !slices.Contains([]string{isolation.Sandbox, isolation.NativeTrusted}, args.IsolationTier) || args.Limit != 0 || args.Offset != 0) || !manage && args.IsolationTier != "" {
		return nil, operatortool.ErrInvalidArguments
	}
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	permission := apikey.ScopeRead
	if manage {
		permission = apikey.ScopeAdmin
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: permission, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	s := t.coordinator.service.server
	scope, err := s.coordinatorScope(ctx, record.ProjectID, manage)
	if err != nil {
		return nil, err
	}
	if manage {
		r, err := s.coordinatorRunner(ctx, scope, args.RunnerID)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(coordinatorRunnerChange{RunnerID: r.RunnerID, IsolationTier: args.IsolationTier, ExpectedRevision: r.Revision})
		if err != nil {
			return nil, err
		}
		description := fmt.Sprintf("Change %s from %s to %s. Sandbox restricts agent access; full access lets agents use this machine's files, credentials and network. Projects requiring sandbox remain ineligible for full-access runners.", r.DisplayName, r.IsolationTier, args.IsolationTier)
		return t.submitCoordinatorAction(ctx, record, call, chat.Action{Kind: chat.ActionKind(call.Name), Title: "Change runner access", Description: description, ProjectID: string(record.ProjectID), Arguments: raw, Material: true})
	}
	ids := []string{args.RunnerID}
	if args.RunnerID == "" {
		limit := args.Limit
		if limit == 0 {
			limit = 20
		}
		rows, err := s.database.db.QueryContext(ctx, `SELECT r.id FROM runner_identities r WHERE r.organization_id=? AND r.removed_at IS NULL AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.organization_id=r.organization_id AND g.project_id=?)) ORDER BY r.id LIMIT ? OFFSET ?`, scope.organization, scope.project, limit, args.Offset)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		ids = nil
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	views := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		r, err := s.coordinatorRunner(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		problems := slices.DeleteFunc(slices.Clone(r.Problems), func(p runnerauth.Problem) bool { return p.ProjectID != "" && p.ProjectID != string(scope.project) })
		views = append(views, map[string]any{"runner_id": r.RunnerID, "display_name": r.DisplayName, "revision": r.Revision, "isolation_tier": r.IsolationTier, "backend_isolation": r.BackendIsolation, "problems": problems, "health": r.Health, "connection_health": r.ConnectionHealth})
	}
	return map[string]any{"runners": views}, nil
}

func (s *Service) executeCoordinatorRunnerAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	scope, err := s.coordinatorScope(ctx, tracker.ProjectID(action.ProjectID), true)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	var change coordinatorRunnerChange
	if decodeCoordinatorArguments(action.Arguments, &change) != nil || change.RunnerID == "" || !slices.Contains([]string{isolation.Sandbox, isolation.NativeTrusted}, change.IsolationTier) {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	r, err := s.coordinatorRunner(ctx, scope, change.RunnerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return chat.ActionExecution{}, nativeNotFound()
		}
		return chat.ActionExecution{}, err
	}
	routing := r.Routing
	routing.ProjectRanks = r.ProjectRankOverrides
	request := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: change.ExpectedRevision, Routing: routing}, IsolationTier: &change.IsolationTier}
	if _, err := s.updateRunnerRoutingCommand(ctx, scope, r.RunnerID, request); err != nil {
		return chat.ActionExecution{}, err
	}
	return chat.ActionExecution{Message: "Runner access updated without re-enrollment. Read get_runners to verify its tier and remaining problems.", ResourceID: r.RunnerID}, nil
}
