package web

import (
	"encoding/json"
	"sync"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
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
	projectID string
	config    string
	issue     string
	identity  agentidentity.Identity
	valid     bool
}

func (s *Server) boardConfiguredAgents(snapshot telemetry.Snapshot) map[string]agentidentity.Identity {
	return s.boardConfiguredAgentsForProject(snapshot, "")
}

func (s *Server) boardConfiguredAgentsForProject(snapshot telemetry.Snapshot, projectID string) map[string]agentidentity.Identity {
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
		inputJSON, err := json.Marshal(input)
		if err != nil {
			continue
		}
		key := templates.BoardIssueKey(issue)
		entry := s.boardIdentities.entries[key]
		if entry != nil && entry.config == configKeys[issue.ProjectID] && entry.issue == string(inputJSON) {
			next[key] = entry
			if entry.valid {
				identities[key] = entry.identity
			}
			continue
		}
		identity, err := resolver.Identity(input)
		next[key] = &boardIdentityEntry{projectID: issue.ProjectID, config: configKeys[issue.ProjectID], issue: string(inputJSON), identity: identity, valid: err == nil}
		if err != nil {
			continue
		}
		identities[key] = identity
	}
	return identities
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
