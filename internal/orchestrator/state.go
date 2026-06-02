package orchestrator

import (
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type State struct {
	PollInterval           time.Duration
	MaxConcurrentAgents    int
	LastRefreshAt          time.Time
	NextRefreshAt          time.Time
	LastRunningReconcileAt time.Time
	Pipeline               []connector.Issue
	Running                map[string]Running
	Claimed                map[string]Claimed
	Blocked                map[string]Blocked
	Completed              map[string]Completed
	Retry                  map[string]Retry
	BudgetRefusals         map[string]BudgetRefusal
	DiffStats              map[string]DiffStats
	CodexTotals            CodexTotals
	RateLimits             *telemetry.RateLimits
}

type Running struct {
	Issue           connector.Issue
	Attempt         int
	StartedAt       time.Time
	WorkerHost      string
	ProcessIdentity string
	SessionID       string
	TurnCount       int
	LastEventAt     time.Time
	LastEvent       string
	LastMessage     string
	RecentEvents    []telemetry.ActivityEvent
	DiffStats       DiffStats
	Tokens          CodexTotals
}

type Claimed struct {
	Issue     connector.Issue
	ClaimedAt time.Time
}

type BlockedSource string

const (
	BlockedSourceDependency    BlockedSource = "dependency"
	BlockedSourceProjectStatus BlockedSource = "project_status"
)

type Blocked struct {
	Issue     connector.Issue
	Reason    string
	BlockedAt time.Time
	Source    BlockedSource
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
		PollInterval:           s.PollInterval,
		MaxConcurrentAgents:    s.MaxConcurrentAgents,
		LastRefreshAt:          s.LastRefreshAt,
		NextRefreshAt:          s.NextRefreshAt,
		LastRunningReconcileAt: s.LastRunningReconcileAt,
		Pipeline:               cloneIssues(s.Pipeline),
		Running:                make(map[string]Running, len(s.Running)),
		Claimed:                make(map[string]Claimed, len(s.Claimed)),
		Blocked:                make(map[string]Blocked, len(s.Blocked)),
		Completed:              make(map[string]Completed, len(s.Completed)),
		Retry:                  make(map[string]Retry, len(s.Retry)),
		BudgetRefusals:         make(map[string]BudgetRefusal, len(s.BudgetRefusals)),
		DiffStats:              make(map[string]DiffStats, len(s.DiffStats)),
		CodexTotals:            s.CodexTotals,
		RateLimits:             cloneRateLimits(s.RateLimits),
	}

	for id, running := range s.Running {
		running.Issue = cloneIssue(running.Issue)
		running.RecentEvents = cloneActivityEvents(running.RecentEvents)
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
	if issue.PRNumber != nil {
		prNumber := *issue.PRNumber
		cloned.PRNumber = &prNumber
	}
	if issue.PullRequest != nil {
		pullRequest := *issue.PullRequest
		cloned.PullRequest = &pullRequest
	}
	if issue.CreatedAt != nil {
		createdAt := *issue.CreatedAt
		cloned.CreatedAt = &createdAt
	}
	if issue.UpdatedAt != nil {
		updatedAt := *issue.UpdatedAt
		cloned.UpdatedAt = &updatedAt
	}
	if issue.StageUpdatedAt != nil {
		stageUpdatedAt := *issue.StageUpdatedAt
		cloned.StageUpdatedAt = &stageUpdatedAt
	}
	cloned.BlockedBy = append([]connector.BlockedRef(nil), issue.BlockedBy...)
	cloned.Labels = cloneStringSlice(issue.Labels)
	cloned.Assignees = cloneStringSlice(issue.Assignees)
	cloned.Fields = cloneStringMap(issue.Fields)
	return cloned
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneActivityEvents(events []telemetry.ActivityEvent) []telemetry.ActivityEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]telemetry.ActivityEvent, len(events))
	copy(out, events)
	return out
}

func cloneRateLimits(rateLimits *telemetry.RateLimits) *telemetry.RateLimits {
	if rateLimits == nil {
		return nil
	}

	cloned := *rateLimits
	cloned.Primary = cloneRateLimitBucket(rateLimits.Primary)
	cloned.Secondary = cloneRateLimitBucket(rateLimits.Secondary)
	cloned.Credits = cloneRateLimitBucket(rateLimits.Credits)
	cloned.GitHubGraphQL = cloneRateLimitBucket(rateLimits.GitHubGraphQL)
	return &cloned
}

func mergeRateLimits(current *telemetry.RateLimits, incoming *telemetry.RateLimits) *telemetry.RateLimits {
	merged := cloneRateLimits(incoming)
	if merged == nil {
		return cloneRateLimits(current)
	}
	if current != nil && current.GitHubGraphQL != nil && merged.GitHubGraphQL == nil {
		merged.GitHubGraphQL = cloneRateLimitBucket(current.GitHubGraphQL)
	}
	return merged
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
