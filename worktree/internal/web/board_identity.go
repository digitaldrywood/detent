package web

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

// boardIdentityCache retains immutable routing inputs for the last rendered issue set.
// Scoped renders replace only their own project entries.
// Body edits and configuration or selector changes invalidate a resolution;
// unrelated telemetry updates do not. The mutex serializes concurrent SSE renders.
type boardIdentityCache struct {
	mu      sync.Mutex
	entries map[string]*boardIdentityEntry
}

type boardIdentityEntry struct {
	projectID     string
	config        string
	issue         string
	identity      agentidentity.Identity
	stageIdentity agentidentity.Identity
	valid         bool
}

func (s *Server) boardConfiguredAgents(snapshot telemetry.Snapshot) map[string]agentidentity.Identity {
	return s.boardConfiguredAgentsForProject(snapshot, "")
}

func (s *Server) boardConfiguredAgentsForProject(snapshot telemetry.Snapshot, projectID string) map[string]agentidentity.Identity {
	identities, _ := s.boardAgentIdentitiesForProject(snapshot, projectID)
	return identities
}

func (s *Server) boardAgentIdentitiesForProject(snapshot telemetry.Snapshot, projectID string) (map[string]agentidentity.Identity, map[string]agentidentity.Identity) {
	s.boardIdentities.mu.Lock()
	defer s.boardIdentities.mu.Unlock()
	next := make(map[string]*boardIdentityEntry)
	if projectID != "" {
		for key, entry := range s.boardIdentities.entries {
			if entry.projectID != projectID {
				next[key] = entry
			}
		}
	}
	defer func() { s.boardIdentities.entries = next }()
	configKeys := make(map[string]string)
	identities := make(map[string]agentidentity.Identity, len(snapshot.BoardIssues))
	stages := make(map[string]agentidentity.Identity, len(snapshot.BoardIssues))
	configs := make(map[string]workflowconfig.Config)
	defaults := s.kanbanWorkflow.WithAgentDefaults(s.currentGlobalConfig().Global.Agents, workflowconfig.AgentBudgetDefaults{})
	resolvers := make(map[string]*runner.BoardIdentityResolver)
	issues := boardIdentityIssues(snapshot)
	for _, issue := range issues {
		resolver, exists := resolvers[issue.ProjectID]
		if !exists {
			cfg := defaults
			tracker := s.connector
			if s.registry != nil {
				if tracked, ok := s.registry.Get(project.ID(issue.ProjectID)); ok {
					cfg = tracked.Workflow().Config
					tracker = tracked.Connector()
				}
			}
			configs[issue.ProjectID] = cfg
			ctx := selector.Context{Persona: cfg.Tracker.Assignee}
			if identifier, ok := tracker.(connector.InstanceIdentifier); ok {
				ctx.InstanceLogin = identifier.InstanceLogin()
			}
			configJSON, err := json.Marshal(struct {
				Config  workflowconfig.Config
				Context selector.Context
			}{cfg, ctx})
			if err != nil {
				continue
			}
			configKeys[issue.ProjectID] = string(configJSON)
			resolver, err = runner.NewBoardIdentityResolver(cfg, ctx)
			if err != nil {
				// Remember invalid project configuration for this snapshot too.
				resolvers[issue.ProjectID] = nil
				continue
			}
			resolvers[issue.ProjectID] = resolver
		}
		if resolver == nil {
			continue
		}
		input := connector.Issue{
			ID: issue.ID, Identifier: issue.Identifier, Description: issue.Description, State: issue.State,
			Labels: issue.Labels, AuthorID: issue.AuthorID, AssigneeID: issue.AssigneeID,
			Assignees: issue.Assignees, Priority: issue.Priority, Fields: issue.Fields, ModelOverride: issue.ModelOverride,
		}
		role := boardSheetRole(snapshot, issue, configs[issue.ProjectID])
		inputJSON, err := json.Marshal(struct {
			Issue        connector.Issue
			Role         string
			DispatchMode string
		}{input, role, issue.DispatchMode})
		if err != nil {
			continue
		}
		key := templates.BoardIssueKey(issue)
		entry := s.boardIdentities.entries[key]
		if entry != nil && entry.config == configKeys[issue.ProjectID] && entry.issue == string(inputJSON) {
			next[key] = entry
			if entry.valid {
				identities[key] = entry.identity
				stages[key] = entry.stageIdentity
			}
			continue
		}
		var identity agentidentity.Identity
		if issue.DispatchMode != "" {
			identity, err = resolver.IdentityForMode(input, issue.DispatchMode)
		} else {
			identity, err = resolver.Identity(input)
		}
		stageIdentity := identity
		if err == nil && role != "" && role != identity.Role {
			var stageErr error
			stageIdentity, stageErr = resolver.IdentityForRole(input, role)
			if stageErr != nil {
				stageIdentity = agentidentity.Identity{Role: role}
			}
		}
		next[key] = &boardIdentityEntry{projectID: issue.ProjectID, config: configKeys[issue.ProjectID], issue: string(inputJSON), identity: identity, stageIdentity: stageIdentity, valid: err == nil}
		if err != nil {
			continue
		}
		identities[key] = identity
		stages[key] = stageIdentity
	}
	return identities, stages
}

// Prefer live role evidence; a ready PR awaiting the validator has a different
// next stage from its last implementation attempt.
func boardSheetRole(snapshot telemetry.Snapshot, issue telemetry.Issue, cfg workflowconfig.Config) string {
	key := templates.BoardIssueKey(issue)
	for _, running := range snapshot.Running {
		if templates.BoardIssueKey(running.Issue) == key && running.RuntimeIdentity.Role != "" {
			return running.RuntimeIdentity.Role
		}
	}
	for _, queued := range snapshot.Queue {
		if templates.BoardIssueKey(queued.Issue) == key && queued.QueueState == telemetry.QueueStateRetrying && queued.RuntimeIdentity.Role != "" {
			return queued.RuntimeIdentity.Role
		}
	}
	for _, attempt := range snapshot.WorkAttempts {
		if templates.BoardIssueKey(telemetry.Issue{ProjectID: attempt.ProjectID, ID: attempt.IssueID, Identifier: attempt.Identifier}) == key && attempt.Status == "running" && attempt.RuntimeIdentity.Role != "" {
			return attempt.RuntimeIdentity.Role
		}
	}
	state := strings.ToLower(strings.TrimSpace(issue.State))
	reviewState := strings.TrimSpace(cfg.Agent.AutoPromote.SourceState)
	if gate.Effective(cfg.Gate).Validator.Enabled && issue.PullRequest != nil && !issue.PullRequest.Draft &&
		(state == "in progress" || state == "human review" || state == "in review" ||
			(reviewState != "" && strings.EqualFold(state, reviewState)) || (state == "rework" && issue.GatePending)) {
		return runner.RoleValidator
	}
	return ""
}

func boardIdentityIssues(snapshot telemetry.Snapshot) map[string]telemetry.Issue {
	// Match board source precedence; runtime rows can exist before tracker rows.
	issues := make(map[string]telemetry.Issue)
	add := func(issue telemetry.Issue) {
		if key := templates.BoardIssueKey(issue); key != "" {
			issues[key] = issue
		}
	}
	for _, attempt := range snapshot.WorkAttempts {
		add(telemetry.Issue{ProjectID: attempt.ProjectID, ID: attempt.IssueID, Identifier: attempt.Identifier})
	}
	for _, row := range snapshot.Completed {
		add(row.Issue)
	}
	for _, issue := range snapshot.BoardIssues {
		add(issue)
	}
	for _, issue := range snapshot.Pipeline {
		add(issue)
	}
	for _, row := range snapshot.Queue {
		add(row.Issue)
	}
	for _, row := range snapshot.Running {
		add(row.Issue)
	}
	for _, row := range snapshot.Blocked {
		add(row.Issue)
	}
	return issues
}
