package web

import (
	"slices"

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
	seen := make(map[string]bool)
	for _, issue := range slices.Concat(snapshot.Pipeline, snapshot.BoardIssues) {
		key := issue.ProjectID + "\x00" + issue.ID
		if seen[key] {
			continue
		}
		seen[key] = true
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
