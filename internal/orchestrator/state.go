package orchestrator

import (
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/telemetry"
)

type State struct {
	PollInterval        time.Duration
	MaxConcurrentAgents int
	Running             map[string]Running
	Claimed             map[string]Claimed
	Blocked             map[string]Blocked
	Completed           map[string]Completed
	Retry               map[string]Retry
	BudgetRefusals      map[string]BudgetRefusal
	DiffStats           map[string]DiffStats
	CodexTotals         CodexTotals
	RateLimits          *telemetry.RateLimits
}

type Running struct {
	Issue      connector.Issue
	Attempt    int
	StartedAt  time.Time
	WorkerHost string
}

type Claimed struct {
	Issue     connector.Issue
	ClaimedAt time.Time
}

type Blocked struct {
	Issue     connector.Issue
	Reason    string
	BlockedAt time.Time
}

type Completed struct {
	Issue       connector.Issue
	StartedAt   time.Time
	CompletedAt time.Time
	FinalState  string
	Tokens      CodexTotals
}

type Retry struct {
	Issue      connector.Issue
	Attempt    int
	DueAt      time.Time
	Error      string
	WorkerHost string
}

func newState(cfg Config) State {
	return State{
		PollInterval:        cfg.PollInterval,
		MaxConcurrentAgents: cfg.MaxConcurrentAgents,
		Running:             map[string]Running{},
		Claimed:             map[string]Claimed{},
		Blocked:             map[string]Blocked{},
		Completed:           map[string]Completed{},
		Retry:               map[string]Retry{},
		BudgetRefusals:      map[string]BudgetRefusal{},
		DiffStats:           map[string]DiffStats{},
	}
}

func (s State) clone() State {
	cloned := State{
		PollInterval:        s.PollInterval,
		MaxConcurrentAgents: s.MaxConcurrentAgents,
		Running:             make(map[string]Running, len(s.Running)),
		Claimed:             make(map[string]Claimed, len(s.Claimed)),
		Blocked:             make(map[string]Blocked, len(s.Blocked)),
		Completed:           make(map[string]Completed, len(s.Completed)),
		Retry:               make(map[string]Retry, len(s.Retry)),
		BudgetRefusals:      make(map[string]BudgetRefusal, len(s.BudgetRefusals)),
		DiffStats:           make(map[string]DiffStats, len(s.DiffStats)),
		CodexTotals:         s.CodexTotals,
		RateLimits:          cloneRateLimits(s.RateLimits),
	}

	for id, running := range s.Running {
		running.Issue = cloneIssue(running.Issue)
		cloned.Running[id] = running
	}
	for id, claimed := range s.Claimed {
		claimed.Issue = cloneIssue(claimed.Issue)
		cloned.Claimed[id] = claimed
	}
	for id, blocked := range s.Blocked {
		blocked.Issue = cloneIssue(blocked.Issue)
		cloned.Blocked[id] = blocked
	}
	for id, completed := range s.Completed {
		completed.Issue = cloneIssue(completed.Issue)
		cloned.Completed[id] = completed
	}
	for id, retry := range s.Retry {
		retry.Issue = cloneIssue(retry.Issue)
		cloned.Retry[id] = retry
	}
	for id, refusal := range s.BudgetRefusals {
		refusal.Issue = cloneIssue(refusal.Issue)
		if refusal.MaxUSD != nil {
			maxUSD := *refusal.MaxUSD
			refusal.MaxUSD = &maxUSD
		}
		if refusal.ResetAt != nil {
			resetAt := *refusal.ResetAt
			refusal.ResetAt = &resetAt
		}
		cloned.BudgetRefusals[id] = refusal
	}
	for id, diffStats := range s.DiffStats {
		cloned.DiffStats[id] = diffStats
	}

	return cloned
}

func cloneIssue(issue connector.Issue) connector.Issue {
	cloned := issue
	if issue.Priority != nil {
		priority := *issue.Priority
		cloned.Priority = &priority
	}
	if issue.CreatedAt != nil {
		createdAt := *issue.CreatedAt
		cloned.CreatedAt = &createdAt
	}
	if issue.UpdatedAt != nil {
		updatedAt := *issue.UpdatedAt
		cloned.UpdatedAt = &updatedAt
	}
	cloned.BlockedBy = append([]connector.BlockedRef(nil), issue.BlockedBy...)
	cloned.Labels = append([]string(nil), issue.Labels...)
	return cloned
}

func cloneRateLimits(rateLimits *telemetry.RateLimits) *telemetry.RateLimits {
	if rateLimits == nil {
		return nil
	}

	cloned := *rateLimits
	cloned.Primary = cloneRateLimitBucket(rateLimits.Primary)
	cloned.Secondary = cloneRateLimitBucket(rateLimits.Secondary)
	cloned.Credits = cloneRateLimitBucket(rateLimits.Credits)
	return &cloned
}

func cloneRateLimitBucket(bucket *telemetry.RateLimitBucket) *telemetry.RateLimitBucket {
	if bucket == nil {
		return nil
	}

	cloned := *bucket
	if bucket.ResetAt != nil {
		resetAt := *bucket.ResetAt
		cloned.ResetAt = &resetAt
	}
	return &cloned
}

func addCodexTotals(left, right CodexTotals) CodexTotals {
	return CodexTotals{
		InputTokens:    left.InputTokens + right.InputTokens,
		OutputTokens:   left.OutputTokens + right.OutputTokens,
		TotalTokens:    left.TotalTokens + right.TotalTokens,
		RuntimeSeconds: left.RuntimeSeconds + right.RuntimeSeconds,
	}
}

func diffStatsPresent(diffStats DiffStats) bool {
	return diffStats.FilesChanged != 0 ||
		diffStats.AddedLines != 0 ||
		diffStats.RemovedLines != 0 ||
		diffStats.Status != ""
}
