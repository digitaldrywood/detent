package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RunnerTools() []runner.AgentTool {
	return []runner.AgentTool{
		Tool("get_runners", "Read runners assigned to this project, including selected isolation tier, backend tier support and current problems. Supply runner_id for one runner; otherwise returns a bounded page.", `{"type":"object","properties":{"runner_id":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
		Tool("convert_runner_scope", "Preview converting an existing runner to all current and future organization projects without reinstalling. Organization owner/admin only; requires user approval. Existing ranks, identity, provider logins and active sessions are kept; each new repository policy still needs approval.", `{"type":"object","required":["runner_id"],"properties":{"runner_id":{"type":"string"}},"additionalProperties":false}`),
		Tool("set_runner_tier", "Preview changing a runner's access tier without re-enrollment. Organization owner/admin only. The user must approve this preview even when ordinary chat confirmation is disabled.", `{"type":"object","required":["runner_id","isolation_tier"],"properties":{"runner_id":{"type":"string"},"isolation_tier":{"type":"string","enum":["sandbox","native-trusted"]}},"additionalProperties":false}`),
	}
}

type RunnerArguments struct {
	RunnerID      string `json:"runner_id"`
	IsolationTier string `json:"isolation_tier,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Offset        int    `json:"offset,omitempty"`
}

type coordinatorRunnerChange struct {
	RunnerID         string `json:"runner_id"`
	IsolationTier    string `json:"isolation_tier,omitempty"`
	Scope            string `json:"scope,omitempty"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (s RunnerDeps) Runner(ctx context.Context, scope hubscope.NativeScope, id string) (runnerauth.Runner, error) {
	runners, err := s.Runners(ctx, scope, RunnerArguments{RunnerID: id})
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if len(runners) != 1 {
		return runnerauth.Runner{}, hubapi.NativeNotFound()
	}
	return runners[0], nil
}

func (s RunnerDeps) Runners(ctx context.Context, scope hubscope.NativeScope, args RunnerArguments) ([]runnerauth.Runner, error) {
	if !s.SnapshotAvailable {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	runners := []runnerauth.Runner{}
	err := s.ReadSnapshot(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := s.Authorize(ctx, r, scope, false); err != nil {
			return err
		}
		now := s.Now()
		var records []runnerstore.RoutingRecord
		if args.RunnerID != "" {
			reader, ok := r.(runnerstore.RoutingReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			record, err := reader.RunnerRoutingRecord(ctx, args.RunnerID, now)
			if err != nil {
				return err
			}
			records = []runnerstore.RoutingRecord{record}
		} else {
			reader, ok := r.(runnerstore.RoutingPageReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			limit := args.Limit
			if limit == 0 {
				limit = 20
			}
			var err error
			records, err = reader.RunnerProjectRoutingRecords(ctx, scope.Project, limit, args.Offset, now)
			if err != nil {
				return err
			}
		}
		urgent, err := r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		for _, record := range records {
			runner := s.View(record, now, urgent)
			if !runner.AllowsProject(scope.Project) {
				return hubapi.NativeNotFound()
			}
			runners = append(runners, runner)
		}
		return nil
	})
	return runners, hubapi.ProjectRecordError(err)
}

func PreviewRunner(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall, deps RunnerPreviewDeps) (any, error) {
	var args RunnerArguments
	if DecodeArguments(call.Arguments, &args) != nil || args.Limit < 0 || args.Limit > 50 || args.Offset < 0 {
		return nil, operatortool.ErrInvalidArguments
	}
	convert := call.Name == "convert_runner_scope"
	manage := call.Name == "set_runner_tier" || convert
	if manage && (args.RunnerID == "" || (!convert && !slices.Contains([]string{isolation.Sandbox, isolation.NativeTrusted}, args.IsolationTier) || convert && args.IsolationTier != "") || args.Limit != 0 || args.Offset != 0) || !manage && args.IsolationTier != "" {
		return nil, operatortool.ErrInvalidArguments
	}
	ctx, err := deps.ActionContext(ctx, record)
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
	s := deps.Runners
	scope, err := s.Scope(ctx, record.ProjectID, manage)
	if err != nil {
		return nil, err
	}
	if !manage {
		scope.Project = record.ProjectID
	}
	if manage {
		r, err := s.Runner(ctx, scope, args.RunnerID)
		if err != nil {
			return nil, err
		}
		change := coordinatorRunnerChange{RunnerID: r.RunnerID, IsolationTier: args.IsolationTier, ExpectedRevision: r.Revision}
		if convert {
			change.Scope = "organization"
		}
		raw, err := json.Marshal(change)
		if err != nil {
			return nil, err
		}
		description := fmt.Sprintf("Change %s from %s to %s. Sandbox restricts agent access; full access lets agents use this machine's files, credentials and network. Projects requiring sandbox remain ineligible for full-access runners.", r.DisplayName, r.IsolationTier, args.IsolationTier)
		title := "Change runner access"
		if convert {
			title = "Convert runner to all projects"
			description = fmt.Sprintf("Convert %s to all organization projects without reinstalling or interrupting active work. Rank overrides and provider logins are kept. Additional repository policies require approval before work can run here.", r.DisplayName)
			projects, err := deps.ReadableProjects(ctx, record)
			if err != nil {
				return nil, err
			}
			details := []string{description}
			for _, project := range projects {
				if r.Scope != "organization" && !slices.Contains(r.ProjectIDs, project) {
					details = append(details, fmt.Sprintf("Review project %s: /settings/projects/%s/workflow", project, url.PathEscape(string(project))))
				}
			}
			description = strings.Join(details, "\n")
		}
		return deps.Submit(ctx, record, call, chat.Action{Kind: chat.ActionKind(call.Name), Title: title, Description: description, ProjectID: string(record.ProjectID), Arguments: raw, Material: true})
	}
	runners, err := s.Runners(ctx, scope, args)
	if err != nil {
		return nil, err
	}
	views := make([]map[string]any, 0, len(runners))
	for _, r := range runners {
		problems := slices.DeleteFunc(slices.Clone(r.Problems), func(p runnerauth.Problem) bool { return p.ProjectID != "" && p.ProjectID != string(scope.Project) })
		views = append(views, map[string]any{"runner_id": r.RunnerID, "display_name": r.DisplayName, "revision": r.Revision, "scope": r.Scope, "isolation_tier": r.IsolationTier, "backend_isolation": r.BackendIsolation, "problems": problems, "health": r.Health, "connection_health": r.ConnectionHealth, "last_heartbeat_at": r.LastHeartbeatAt})
	}
	return map[string]any{"runners": views, "observed_at": s.Now().UTC()}, nil
}

func (s RunnerDeps) Execute(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	scope, err := s.Scope(ctx, tracker.ProjectID(action.ProjectID), true)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	var change coordinatorRunnerChange
	if DecodeArguments(action.Arguments, &change) != nil || change.RunnerID == "" || (string(action.Kind) == "convert_runner_scope" && (change.Scope != "organization" || change.IsolationTier != "") || string(action.Kind) == "set_runner_tier" && (!slices.Contains([]string{isolation.Sandbox, isolation.NativeTrusted}, change.IsolationTier) || change.Scope != "")) {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	r, err := s.Runner(ctx, scope, change.RunnerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return chat.ActionExecution{}, hubapi.NativeNotFound()
		}
		return chat.ActionExecution{}, err
	}
	routing := r.Routing
	routing.ProjectRanks = r.ProjectRankOverrides
	request := runnerauth.RoutingChange{ExpectedRevision: change.ExpectedRevision, Routing: routing}
	tier := &change.IsolationTier
	if string(action.Kind) == "convert_runner_scope" {
		tier = nil
		request.Scope = "organization"
		request.ProjectIDs = nil
	}
	if err := s.UpdateRouting(ctx, scope, r.RunnerID, request, tier); err != nil {
		return chat.ActionExecution{}, err
	}
	if string(action.Kind) == "convert_runner_scope" {
		return chat.ActionExecution{Message: "Runner converted in place to all organization projects. Additional repository policies still require approval in project settings; no policies were approved by this conversion.", ResourceID: r.RunnerID}, nil
	}
	return chat.ActionExecution{Message: "Runner access updated without re-enrollment. Read get_runners to verify its tier and remaining problems.", ResourceID: r.RunnerID}, nil
}
