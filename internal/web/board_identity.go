package web

import (
	"github.com/digitaldrywood/detent/internal/agentidentity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) boardConfiguredAgents(snapshot telemetry.Snapshot) map[string]agentidentity.Identity {
	identities := make(map[string]agentidentity.Identity, len(snapshot.BoardIssues))
	defaults := s.kanbanWorkflow.WithAgentDefaults(s.currentGlobalConfig().Global.Agents, workflowconfig.AgentBudgetDefaults{})
	resolvers := make(map[string]*runner.BoardIdentityResolver)
	// Match board source precedence; runtime rows can exist before tracker rows.
	issues := make(map[string]telemetry.Issue)
	add := func(issue telemetry.Issue) { issues[issue.ProjectID+"\x00"+issue.ID] = issue }
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
			var err error
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
		identity, err := resolver.Identity(connector.Issue{
			ID: issue.ID, Identifier: issue.Identifier, Description: issue.Description, State: issue.State,
			Labels: issue.Labels, AuthorID: issue.AuthorID, AssigneeID: issue.AssigneeID,
			Assignees: issue.Assignees, Priority: issue.Priority, Fields: issue.Fields,
		})
		if err != nil {
			continue
		}
		identities[issue.ProjectID+"\x00"+issue.ID] = identity
	}
	return identities
}
