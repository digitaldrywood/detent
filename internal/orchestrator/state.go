package orchestrator

import (
	"context"
	"maps"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type State struct {
	PollInterval             time.Duration
	MaxConcurrentAgents      int
	AutoPromoteQuietDuration time.Duration
	AutoPromote              AutoPromoteConfig
	ActiveStates             []string
	TerminalStates           []string
	Instance                 telemetry.Instance
	Authorization            selector.Selector
	SelectorContext          selector.Context
	Draining                 bool
	DrainStartedAt           time.Time
	DataSeq                  uint64
	LastRefreshAt            time.Time
	NextRefreshAt            time.Time
	LastRefreshError         string
	LastRefreshErrorAt       time.Time
	ManualRefresh            telemetry.RefreshAttempt
	LastRunningReconcileAt   time.Time
	LastWorkspaceCleanupAt   time.Time
	RecentEvents             []telemetry.ActivityEvent
	Auth                     connector.AuthHealth
	StatusDrift              connector.StatusDrift
	BoardIssues              []connector.Issue
	Pipeline                 []connector.Issue
	Running                  map[string]Running
	WorkAttempts             []telemetry.WorkAttempt
	SchedulerDecisions       []telemetry.SchedulerDecision
	Claimed                  map[string]Claimed
	Blocked                  map[string]Blocked
	Completed                map[string]Completed
	Retry                    map[string]Retry
	MergeTimings             map[string]MergeTiming
	TransientCheckRetries    map[string]TransientCheckRetry
	BudgetRefusals           map[string]BudgetRefusal
	PriorAttempts            map[string]runpkg.PriorAttempt
	InstantFailures          map[string]InstantFailure
	DiffStats                map[string]DiffStats
	ReapedWorkspaces         map[string]time.Time
	TokenTotals              TokenTotals
	RateLimits               *telemetry.RateLimits
	planRework               map[string]struct{}
	epicTransitionWatch      []connector.Issue
	pendingEpicParentLookups map[string]connector.Issue
}

type Running struct {
	Issue                 connector.Issue
	Attempt               int
	WorkAttemptID         int64
	Mode                  string
	StartedAt             time.Time
	WorkerHost            string
	ProcessIdentity       string
	WorkspacePath         string
	SessionID             string
	TurnCount             int
	LastEventAt           time.Time
	LastEvent             string
	LastMessage           string
	LastMessageTruncation *runtimeoutput.Truncation
	RecentEvents          []telemetry.ActivityEvent
	DiffStats             DiffStats
	Tokens                TokenTotals
	globalSlot            scheduler.Slot
	cancel                context.CancelFunc
}

type Claimed struct {
	Issue          connector.Issue
	ClaimedAt      time.Time
	Owner          string
	LeaseRenewedAt time.Time
	LeaseExpiresAt time.Time
}

type BlockedSource = telemetry.BlockedSource

const (
	BlockedSourceDependency    = telemetry.BlockedSourceDependency
	BlockedSourceProjectStatus = telemetry.BlockedSourceProjectStatus
)

type Blocked struct {
	Issue          connector.Issue
	Reason         string
	RecoveryReason string
	RecoveryTarget string
	BlockedAt      time.Time
	Source         BlockedSource
}

type Completed struct {
	Issue       connector.Issue
	StartedAt   time.Time
	CompletedAt time.Time
	FinalState  string
	Tokens      TokenTotals
	MergeTiming MergeTiming
}

type MergeTiming struct {
	EnteredMergingAt           time.Time
	MergeWorkerSlotAcquiredAt  time.Time
	MergeStartedAt             time.Time
	BaseRefreshStartedAt       time.Time
	BaseRefreshFinishedAt      time.Time
	CIWaitStartedAt            time.Time
	CIWaitFinishedAt           time.Time
	MergedAt                   time.Time
	MergeFailedAt              time.Time
	MergeFailureReason         string
	QueueWaitSeconds           int64
	ActiveMergeDurationSeconds int64
	TotalMergingSeconds        int64
}

type Retry struct {
	Issue      connector.Issue
	Attempt    int
	DueAt      time.Time
	Error      string
	WorkerHost string
}

type InstantFailure struct {
	Issue          connector.Issue
	Error          string
	errorKey       string
	Count          int
	FirstFailureAt time.Time
	LastFailureAt  time.Time
}

type TransientCheckRetry struct {
	IssueID       string
	HeadSHA       string
	CheckName     string
	CheckID       int64
	WorkflowRunID int64
	Attempts      int
	RetriedAt     time.Time
}

func newState(cfg Config) State {
	return State{
		PollInterval:             cfg.PollInterval,
		MaxConcurrentAgents:      cfg.MaxConcurrentAgents,
		AutoPromoteQuietDuration: cfg.AutoPromote.QuietDuration,
		AutoPromote:              cloneAutoPromoteConfig(cfg.AutoPromote),
		ActiveStates:             append([]string(nil), cfg.ActiveStates...),
		TerminalStates:           append([]string(nil), cfg.TerminalStates...),
		Instance:                 instanceSnapshot(cfg),
		Authorization:            cloneSelector(cfg.Authorization),
		SelectorContext:          cfg.SelectorContext,
		Running:                  map[string]Running{},
		Claimed:                  map[string]Claimed{},
		Blocked:                  map[string]Blocked{},
		Completed:                map[string]Completed{},
		Retry:                    map[string]Retry{},
		MergeTimings:             map[string]MergeTiming{},
		TransientCheckRetries:    map[string]TransientCheckRetry{},
		BudgetRefusals:           map[string]BudgetRefusal{},
		PriorAttempts:            map[string]runpkg.PriorAttempt{},
		InstantFailures:          map[string]InstantFailure{},
		DiffStats:                map[string]DiffStats{},
		ReapedWorkspaces:         map[string]time.Time{},
		planRework:               map[string]struct{}{},
		pendingEpicParentLookups: map[string]connector.Issue{},
	}
}

func (s State) clone() State {
	cloned := State{
		PollInterval:             s.PollInterval,
		MaxConcurrentAgents:      s.MaxConcurrentAgents,
		AutoPromoteQuietDuration: s.AutoPromoteQuietDuration,
		AutoPromote:              cloneAutoPromoteConfig(s.AutoPromote),
		ActiveStates:             append([]string(nil), s.ActiveStates...),
		TerminalStates:           append([]string(nil), s.TerminalStates...),
		Instance:                 s.Instance,
		Authorization:            cloneSelector(s.Authorization),
		SelectorContext:          s.SelectorContext,
		Draining:                 s.Draining,
		DrainStartedAt:           s.DrainStartedAt,
		DataSeq:                  s.DataSeq,
		LastRefreshAt:            s.LastRefreshAt,
		NextRefreshAt:            s.NextRefreshAt,
		LastRefreshError:         s.LastRefreshError,
		LastRefreshErrorAt:       s.LastRefreshErrorAt,
		ManualRefresh:            cloneRefreshAttempt(s.ManualRefresh),
		LastRunningReconcileAt:   s.LastRunningReconcileAt,
		LastWorkspaceCleanupAt:   s.LastWorkspaceCleanupAt,
		RecentEvents:             cloneActivityEvents(s.RecentEvents),
		Auth:                     s.Auth,
		StatusDrift:              cloneStatusDrift(s.StatusDrift),
		BoardIssues:              cloneIssues(s.BoardIssues),
		Pipeline:                 cloneIssues(s.Pipeline),
		WorkAttempts:             cloneTelemetryWorkAttempts(s.WorkAttempts),
		SchedulerDecisions:       cloneTelemetrySchedulerDecisions(s.SchedulerDecisions),
		Running:                  make(map[string]Running, len(s.Running)),
		Claimed:                  make(map[string]Claimed, len(s.Claimed)),
		Blocked:                  make(map[string]Blocked, len(s.Blocked)),
		Completed:                make(map[string]Completed, len(s.Completed)),
		Retry:                    make(map[string]Retry, len(s.Retry)),
		MergeTimings:             maps.Clone(s.MergeTimings),
		TransientCheckRetries:    maps.Clone(s.TransientCheckRetries),
		BudgetRefusals:           make(map[string]BudgetRefusal, len(s.BudgetRefusals)),
		PriorAttempts:            clonePriorAttempts(s.PriorAttempts),
		InstantFailures:          make(map[string]InstantFailure, len(s.InstantFailures)),
		DiffStats:                make(map[string]DiffStats, len(s.DiffStats)),
		ReapedWorkspaces:         make(map[string]time.Time, len(s.ReapedWorkspaces)),
		TokenTotals:              s.TokenTotals,
		RateLimits:               cloneRateLimits(s.RateLimits),
		planRework:               make(map[string]struct{}, len(s.planRework)),
		epicTransitionWatch:      cloneIssues(s.epicTransitionWatch),
		pendingEpicParentLookups: cloneIssueMap(s.pendingEpicParentLookups),
	}

	for id, running := range s.Running {
		running.Issue = cloneIssue(running.Issue)
		running.LastMessageTruncation = runtimeoutput.CloneTruncation(running.LastMessageTruncation)
		running.RecentEvents = cloneActivityEvents(running.RecentEvents)
		running.globalSlot = scheduler.Slot{}
		running.cancel = nil
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
	for id, failure := range s.InstantFailures {
		failure.Issue = cloneIssue(failure.Issue)
		cloned.InstantFailures[id] = failure
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
	maps.Copy(cloned.DiffStats, s.DiffStats)
	maps.Copy(cloned.ReapedWorkspaces, s.ReapedWorkspaces)
	maps.Copy(cloned.planRework, s.planRework)

	return cloned
}

func cloneAutoPromoteConfig(cfg AutoPromoteConfig) AutoPromoteConfig {
	cfg.AllowedIssueLabels = append([]string(nil), cfg.AllowedIssueLabels...)
	cfg.Gate = cloneGateConfig(cfg.Gate)
	return cfg
}

func cloneGateConfig(cfg gate.Config) gate.Config {
	cfg.RequiredStatusChecks = append([]string(nil), cfg.RequiredStatusChecks...)
	cfg.RequireAutomatedReview = cloneBoolPointer(cfg.RequireAutomatedReview)
	cfg.TransientCIRetryLimit = cloneIntPointer(cfg.TransientCIRetryLimit)
	cfg.Validator.BlockOn = append([]string(nil), cfg.Validator.BlockOn...)
	cfg.Validator.MaxInlineDiffBytes = cloneIntPointer(cfg.Validator.MaxInlineDiffBytes)
	cfg.Artifact.PassStatuses = append([]string(nil), cfg.Artifact.PassStatuses...)
	cfg.Artifact.WaitStatuses = append([]string(nil), cfg.Artifact.WaitStatuses...)
	cfg.Artifact.ReworkStatuses = append([]string(nil), cfg.Artifact.ReworkStatuses...)
	return cfg
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneSelector(in selector.Selector) selector.Selector {
	out := in
	out.AssigneeIn = cloneStringSlice(in.AssigneeIn)
	out.AuthorIn = cloneStringSlice(in.AuthorIn)
	out.PriorityIn = append([]int(nil), in.PriorityIn...)
	out.Labels.Include = cloneStringSlice(in.Labels.Include)
	out.Labels.Exclude = cloneStringSlice(in.Labels.Exclude)
	out.Fields = append([]selector.FieldEquals(nil), in.Fields...)
	out.And = cloneSelectors(in.And)
	out.Or = cloneSelectors(in.Or)
	return out
}

func cloneSelectors(in []selector.Selector) []selector.Selector {
	if len(in) == 0 {
		return nil
	}
	out := make([]selector.Selector, 0, len(in))
	for _, item := range in {
		out = append(out, cloneSelector(item))
	}
	return out
}

func clonePriorAttempts(in map[string]runpkg.PriorAttempt) map[string]runpkg.PriorAttempt {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]runpkg.PriorAttempt, len(in))
	for key, value := range in {
		value.Validator.Findings = append([]gate.Finding(nil), value.Validator.Findings...)
		out[key] = value
	}
	return out
}

func cloneStatusDrift(drift connector.StatusDrift) connector.StatusDrift {
	return connector.StatusDrift{
		UntrackedOpen: cloneIssues(drift.UntrackedOpen),
		OpenTerminal:  cloneIssues(drift.OpenTerminal),
	}
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
		if issue.PullRequest.ActivityAt != nil {
			activityAt := *issue.PullRequest.ActivityAt
			pullRequest.ActivityAt = &activityAt
		}
		if issue.PullRequest.CodexReviewSubmittedAt != nil {
			submittedAt := *issue.PullRequest.CodexReviewSubmittedAt
			pullRequest.CodexReviewSubmittedAt = &submittedAt
		}
		if issue.PullRequest.LatestCodexReviewSubmittedAt != nil {
			submittedAt := *issue.PullRequest.LatestCodexReviewSubmittedAt
			pullRequest.LatestCodexReviewSubmittedAt = &submittedAt
		}
		if issue.PullRequest.HydrationNextRetryAt != nil {
			nextRetryAt := *issue.PullRequest.HydrationNextRetryAt
			pullRequest.HydrationNextRetryAt = &nextRetryAt
		}
		pullRequest.SlowChecks = append([]connector.PullRequestCheck(nil), issue.PullRequest.SlowChecks...)
		pullRequest.RunningChecks = append([]string(nil), issue.PullRequest.RunningChecks...)
		pullRequest.StaleSuccessfulChecks = append([]connector.PullRequestCheck(nil), issue.PullRequest.StaleSuccessfulChecks...)
		pullRequest.RequiredCheckFailures = append([]connector.PullRequestCheck(nil), issue.PullRequest.RequiredCheckFailures...)
		pullRequest.TransientFailedChecks = append([]connector.PullRequestCheck(nil), issue.PullRequest.TransientFailedChecks...)
		pullRequest.CodexReviewFindings = append([]connector.PullRequestFinding(nil), issue.PullRequest.CodexReviewFindings...)
		cloned.PullRequest = &pullRequest
	}
	if issue.Deliverable != nil {
		deliverable := *issue.Deliverable
		deliverable.Metadata = cloneStringMap(issue.Deliverable.Metadata)
		cloned.Deliverable = &deliverable
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
	cloned.ChildIssues = append([]connector.BlockedRef(nil), issue.ChildIssues...)
	cloned.Labels = cloneStringSlice(issue.Labels)
	cloned.Comments = cloneIssueComments(issue.Comments)
	cloned.Assignees = cloneStringSlice(issue.Assignees)
	cloned.Fields = cloneStringMap(issue.Fields)
	cloned.Metadata = cloneStringMap(issue.Metadata)
	return cloned
}

func cloneRefreshAttempt(attempt telemetry.RefreshAttempt) telemetry.RefreshAttempt {
	cloned := attempt
	cloned.RequestedAt = timePointerValue(attempt.RequestedAt)
	cloned.StartedAt = timePointerValue(attempt.StartedAt)
	cloned.CompletedAt = timePointerValue(attempt.CompletedAt)
	cloned.LastErrorAt = timePointerValue(attempt.LastErrorAt)
	cloned.Operations = append([]string(nil), attempt.Operations...)
	return cloned
}

func timePointerValue(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func cloneIssueMap(issues map[string]connector.Issue) map[string]connector.Issue {
	if len(issues) == 0 {
		return nil
	}
	out := make(map[string]connector.Issue, len(issues))
	for key, issue := range issues {
		out[key] = cloneIssue(issue)
	}
	return out
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func cloneIssueComments(comments []connector.IssueComment) []connector.IssueComment {
	if comments == nil {
		return nil
	}
	out := make([]connector.IssueComment, len(comments))
	for index, comment := range comments {
		out[index] = comment
		out[index].CreatedAt = timePointerValue(comment.CreatedAt)
		out[index].UpdatedAt = timePointerValue(comment.UpdatedAt)
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	maps.Copy(out, values)
	return out
}

func cloneActivityEvents(events []telemetry.ActivityEvent) []telemetry.ActivityEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]telemetry.ActivityEvent, len(events))
	copy(out, events)
	for index := range out {
		out[index].Truncation = runtimeoutput.CloneTruncation(out[index].Truncation)
	}
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
	cloned.GitHubREST = cloneRateLimitBucket(rateLimits.GitHubREST)
	cloned.GraphQLCost = cloneGraphQLCost(rateLimits.GraphQLCost)
	cloned.RESTUsage = cloneRESTUsage(rateLimits.RESTUsage)
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
	if current != nil && current.GraphQLCost != nil && merged.GraphQLCost == nil {
		merged.GraphQLCost = cloneGraphQLCost(current.GraphQLCost)
	}
	if current != nil && current.GitHubREST != nil && merged.GitHubREST == nil {
		merged.GitHubREST = cloneRateLimitBucket(current.GitHubREST)
	}
	if current != nil && current.RESTUsage != nil && merged.RESTUsage == nil {
		merged.RESTUsage = cloneRESTUsage(current.RESTUsage)
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
	if bucket.ObservedAt != nil {
		observedAt := *bucket.ObservedAt
		cloned.ObservedAt = &observedAt
	}
	return &cloned
}

func cloneGraphQLCost(cost *telemetry.GraphQLCost) *telemetry.GraphQLCost {
	if cost == nil {
		return nil
	}

	cloned := *cost
	if len(cost.Contributors) > 0 {
		cloned.Contributors = append([]telemetry.GraphQLCostContributor(nil), cost.Contributors...)
	}
	return &cloned
}

func cloneRESTUsage(usage *telemetry.RESTUsage) *telemetry.RESTUsage {
	if usage == nil {
		return nil
	}

	cloned := *usage
	if usage.BackoffUntil != nil {
		backoffUntil := *usage.BackoffUntil
		cloned.BackoffUntil = &backoffUntil
	}
	if len(usage.Contributors) > 0 {
		cloned.Contributors = append([]telemetry.RESTUsageContributor(nil), usage.Contributors...)
		for index := range cloned.Contributors {
			if usage.Contributors[index].ResetAt == nil {
				continue
			}
			resetAt := *usage.Contributors[index].ResetAt
			cloned.Contributors[index].ResetAt = &resetAt
		}
	}
	return &cloned
}

func cloneTelemetryWorkAttempts(values []telemetry.WorkAttempt) []telemetry.WorkAttempt {
	if len(values) == 0 {
		return nil
	}
	return append([]telemetry.WorkAttempt(nil), values...)
}

func cloneTelemetrySchedulerDecisions(values []telemetry.SchedulerDecision) []telemetry.SchedulerDecision {
	if len(values) == 0 {
		return nil
	}
	return append([]telemetry.SchedulerDecision(nil), values...)
}

func addTokenTotals(left, right TokenTotals) TokenTotals {
	return TokenTotals{
		InputTokens:           left.InputTokens + right.InputTokens,
		CachedInputTokens:     left.CachedInputTokens + right.CachedInputTokens,
		OutputTokens:          left.OutputTokens + right.OutputTokens,
		ReasoningOutputTokens: left.ReasoningOutputTokens + right.ReasoningOutputTokens,
		TotalTokens:           left.TotalTokens + right.TotalTokens,
		ModelContextWindow:    maxModelContextWindow(left.ModelContextWindow, right.ModelContextWindow),
		RuntimeSeconds:        left.RuntimeSeconds + right.RuntimeSeconds,
	}
}

func maxModelContextWindow(left *int64, right *int64) *int64 {
	switch {
	case left == nil && right == nil:
		return nil
	case left == nil:
		value := *right
		return &value
	case right == nil:
		value := *left
		return &value
	case *right > *left:
		value := *right
		return &value
	default:
		value := *left
		return &value
	}
}

func diffStatsPresent(diffStats DiffStats) bool {
	return diffStats.FilesChanged != 0 ||
		diffStats.AddedLines != 0 ||
		diffStats.RemovedLines != 0 ||
		diffStats.Status != ""
}
