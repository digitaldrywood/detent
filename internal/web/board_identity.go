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
	for _, issue := range snapshot.BoardIssues {
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
		identity, err := runner.ConfiguredBoardIdentity(cfg, connector.Issue{
			ID: issue.ID, Identifier: issue.Identifier, Description: issue.Description,
			Labels: issue.Labels, AuthorID: issue.AuthorID, AssigneeID: issue.AssigneeID,
			Assignees: issue.Assignees, Priority: issue.Priority, Fields: issue.Fields,
		}, ctx)
		if err != nil {
			continue
		}
		identities[issue.ProjectID+"\x00"+issue.ID] = identity
	}
	return identities
}
