package linear

import (
	"context"
	"fmt"
	"strings"
)

const issueWorkflowStatesQuery = `
query DetentLinearIssueWorkflowStates($issueId: String!) {
  issue(id: $issueId) {
    id
    team {
      id
      states(first: 250) {
        nodes {
          id
          name
          type
        }
      }
    }
  }
}`

func (c *Connector) resolveStateID(ctx context.Context, issueID string, state string) (string, error) {
	stateName := c.detentToLinearState(state)
	stateKey := normalizeStateName(stateName)
	if stateKey == "" {
		return "", fmt.Errorf("%w: %s", ErrStateNotFound, strings.TrimSpace(state))
	}

	if stateID, ok := c.cachedStateID(issueID, stateKey); ok && stateID != "" {
		return stateID, nil
	}

	teamID, stateIDs, err := c.fetchIssueWorkflowStateIDs(ctx, issueID)
	if err != nil {
		return "", err
	}
	c.cacheIssueWorkflowStateIDs(issueID, teamID, stateIDs)
	if stateID := stateIDs[stateKey]; stateID != "" {
		return stateID, nil
	}

	return "", fmt.Errorf("%w: %s", ErrStateNotFound, stateName)
}

func (c *Connector) cachedStateID(issueID string, stateKey string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	teamID := c.teamIDByIssue[issueID]
	if teamID == "" {
		return "", false
	}
	states := c.stateIDByTeam[teamID]
	if states == nil {
		return "", false
	}
	return states[stateKey], true
}

func (c *Connector) cachedStateIDForState(issueID string, state string) (string, bool) {
	stateKey := normalizeStateName(c.detentToLinearState(state))
	if stateKey == "" {
		return "", false
	}
	stateID, ok := c.cachedStateID(issueID, stateKey)
	return stateID, ok && stateID != ""
}

func (c *Connector) invalidateIssueStateCache(issueID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.teamIDByIssue, issueID)
}

func (c *Connector) cacheIssueWorkflowStateIDs(issueID string, teamID string, stateIDs map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.teamIDByIssue == nil {
		c.teamIDByIssue = make(map[string]string)
	}
	if c.stateIDByTeam == nil {
		c.stateIDByTeam = make(map[string]map[string]string)
	}
	c.teamIDByIssue[issueID] = teamID
	c.stateIDByTeam[teamID] = stateIDs
}

func (c *Connector) fetchIssueWorkflowStateIDs(ctx context.Context, issueID string) (string, map[string]string, error) {
	var response struct {
		Issue *struct {
			ID   string `json:"id"`
			Team *struct {
				ID     string `json:"id"`
				States struct {
					Nodes []linearWorkflowState `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := c.client.GraphQL(ctx, issueWorkflowStatesQuery, map[string]any{
		"issueId": issueID,
	}, &response); err != nil {
		return "", nil, err
	}
	if response.Issue == nil {
		return "", nil, fmt.Errorf("%w: %s", ErrIssueNotFound, issueID)
	}
	if response.Issue.Team == nil {
		return "", nil, ErrInvalidResponse
	}

	teamID := strings.TrimSpace(response.Issue.Team.ID)
	if teamID == "" {
		return "", nil, ErrInvalidResponse
	}

	stateIDs := make(map[string]string, len(response.Issue.Team.States.Nodes))
	for _, state := range response.Issue.Team.States.Nodes {
		nameKey := normalizeStateName(state.Name)
		stateID := strings.TrimSpace(state.ID)
		if nameKey != "" && stateID != "" {
			stateIDs[nameKey] = stateID
		}
	}

	return teamID, stateIDs, nil
}

func (c *Connector) detentToLinearState(state string) string {
	state = strings.TrimSpace(state)
	if mapped, ok := c.stateMap[state]; ok {
		return strings.TrimSpace(mapped)
	}

	stateKey := normalizeStateName(state)
	for detentState, mapped := range c.stateMap {
		if normalizeStateName(detentState) == stateKey {
			return strings.TrimSpace(mapped)
		}
	}
	return state
}

func cloneStateMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			cloned[key] = value
		}
	}
	return cloned
}

func normalizeStateName(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

type linearWorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
