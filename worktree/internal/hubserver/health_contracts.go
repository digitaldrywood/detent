package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthContractKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	item         string
}

type healthContractResult struct {
	revision tracker.Revision
	policyID string
	wait     *healthWait
}

type healthContractCache struct {
	mu      sync.Mutex
	entries map[healthContractKey]healthContractResult
	order   []healthContractKey
	oldest  int
}

func readHealthContractPolicyID(ctx context.Context, q nativeQueryer, scope nativeScope) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT r.policy_id FROM project_policies p
 JOIN policy_revisions r ON r.scope=p.scope AND r.policy_id=p.policy_id WHERE p.scope=?`, string(scope.organization)+"/"+string(scope.project)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (c *healthContractCache) read(ctx context.Context, q nativeQueryer, scope nativeScope, item string, revision tracker.Revision, policyID string) (*healthWait, error) {
	key := healthContractKey{organization: scope.organization, project: scope.project, item: item}
	if c != nil {
		c.mu.Lock()
		cached, found := c.entries[key]
		c.mu.Unlock()
		if found && cached.revision == revision && cached.policyID == policyID {
			return cached.wait, nil
		}
	}
	issue, _, err := readNativeIssue(ctx, q, scope, item)
	if err != nil {
		return nil, err
	}
	evaluation, err := evaluateNativeIssueContract(ctx, q, scope, issue)
	if err != nil {
		return nil, err
	}
	var wait *healthWait
	if evaluation.ReasonCode == "issue_contract_unsatisfied" {
		wait = &healthWait{Item: item, Project: string(scope.project), Since: issue.UpdatedAt, Active: true, Reason: evaluation.Reason}
	}
	if c == nil || issue.LinkedSource != nil || issue.IssueContract != nil && issue.IssueContract.ReleaseDelivery != nil {
		return wait, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[healthContractKey]healthContractResult)
	}
	if _, exists := c.entries[key]; !exists {
		if len(c.order) == healthReadLimit {
			delete(c.entries, c.order[c.oldest])
			c.order[c.oldest] = key
			c.oldest = (c.oldest + 1) % healthReadLimit
		} else {
			c.order = append(c.order, key)
		}
	}
	c.entries[key] = healthContractResult{revision: revision, policyID: policyID, wait: wait}
	return wait, nil
}
