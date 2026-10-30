package hubhealth

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type contractKey struct {
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
	Item         string
}

type contractResult struct {
	Revision tracker.Revision
	policyID string
	wait     *Wait
}

type ContractCache struct {
	mu      sync.Mutex
	entries map[contractKey]contractResult
	order   []contractKey
	oldest  int
}

func ReadContractPolicyID(ctx context.Context, q hubsql.Queryer, scope Scope) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT r.policy_id FROM project_policies p
 JOIN policy_revisions r ON r.scope=p.scope AND r.policy_id=p.policy_id WHERE p.scope=?`, string(scope.Organization)+"/"+string(scope.Project)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (c *ContractCache) Read(ctx context.Context, q hubsql.Queryer, scope Scope, item string, revision tracker.Revision, policyID string, load ContractLoader) (*Wait, error) {
	key := contractKey{Organization: scope.Organization, Project: scope.Project, Item: item}
	if c != nil {
		c.mu.Lock()
		cached, found := c.entries[key]
		c.mu.Unlock()
		if found && cached.Revision == revision && cached.policyID == policyID {
			return cached.wait, nil
		}
	}
	observation, err := load(ctx, q, scope, item)
	if err != nil {
		return nil, err
	}
	issue := observation.Issue
	evaluation := observation
	var wait *Wait
	if evaluation.ReasonCode == "issue_contract_unsatisfied" {
		wait = &Wait{Item: item, Project: string(scope.Project), Since: issue.UpdatedAt, Active: true, Reason: evaluation.Reason}
	}
	if c == nil || !observation.Cacheable {
		return wait, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[contractKey]contractResult)
	}
	if _, exists := c.entries[key]; !exists {
		if len(c.order) == ReadLimit {
			delete(c.entries, c.order[c.oldest])
			c.order[c.oldest] = key
			c.oldest = (c.oldest + 1) % ReadLimit
		} else {
			c.order = append(c.order, key)
		}
	}
	c.entries[key] = contractResult{Revision: revision, policyID: policyID, wait: wait}
	return wait, nil
}
