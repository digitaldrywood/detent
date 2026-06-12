package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	DependencyReadinessTerminal         = workflowconfig.DependencyReadinessTerminal
	DependencyReadinessTerminalOrMerged = workflowconfig.DependencyReadinessTerminalOrMerged
)

type DependencyAutoUnblockConfig struct {
	Enabled      bool
	SourceStates []string
	TargetState  string
	Readiness    string
}

type dependencyBlocker struct {
	Ref      connector.BlockedRef
	Issue    connector.Issue
	Resolved bool
}

func normalizeDependencyAutoUnblockConfig(cfg DependencyAutoUnblockConfig) DependencyAutoUnblockConfig {
	cfg.SourceStates = normalizedStates(defaultStringSlice(cfg.SourceStates, []string{blockedStatusState}))
	cfg.TargetState = strings.TrimSpace(defaultString(cfg.TargetState, "Todo"))
	cfg.Readiness = strings.ToLower(strings.TrimSpace(defaultString(cfg.Readiness, DependencyReadinessTerminalOrMerged)))
	return cfg
}

func (o *Orchestrator) autoUnblockDependencyIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	cfg := normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock)
	if !cfg.Enabled {
		return nil
	}

	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, cfg.SourceStates) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" || len(issue.BlockedBy) == 0 {
			continue
		}
		blockers := o.resolveDependencyBlockers(ctx, issue)
		if !dependencyBlockersReady(blockers, cfg, o.cfg.TerminalStates) {
			continue
		}
		if !o.applyDependencyAutoUnblock(ctx, state, issue, blockers, cfg.TargetState, now) {
			continue
		}
		transitioned[issueID] = struct{}{}
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func (o *Orchestrator) resolveDependencyBlockers(ctx context.Context, issue connector.Issue) []dependencyBlocker {
	blockers := make([]dependencyBlocker, 0, len(issue.BlockedBy))
	identifiers := make([]string, 0, len(issue.BlockedBy))
	seen := map[string]struct{}{}
	for _, ref := range issue.BlockedBy {
		ref.Identifier = strings.TrimSpace(ref.Identifier)
		ref.ID = strings.TrimSpace(ref.ID)
		ref.State = strings.TrimSpace(ref.State)
		blockers = append(blockers, dependencyBlocker{Ref: ref})
		if ref.Identifier == "" {
			continue
		}
		key := strings.ToLower(ref.Identifier)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		identifiers = append(identifiers, ref.Identifier)
	}

	resolver, ok := o.connector.(connector.IssueReferenceResolver)
	if !ok || len(identifiers) == 0 {
		return blockers
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("resolve dependency blockers failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return blockers
	}

	byIdentifier := make(map[string]connector.Issue, len(issues))
	for _, blocker := range issues {
		identifier := strings.ToLower(strings.TrimSpace(blocker.Identifier))
		if identifier == "" {
			continue
		}
		byIdentifier[identifier] = blocker
	}
	for index := range blockers {
		identifier := strings.ToLower(strings.TrimSpace(blockers[index].Ref.Identifier))
		blocker, ok := byIdentifier[identifier]
		if !ok {
			continue
		}
		blockers[index].Issue = blocker
		blockers[index].Resolved = true
		blockers[index].Ref.ID = firstNonBlank(blocker.ID, blockers[index].Ref.ID)
		blockers[index].Ref.Identifier = firstNonBlank(blocker.Identifier, blockers[index].Ref.Identifier)
		blockers[index].Ref.State = firstNonBlank(blocker.State, blockers[index].Ref.State)
	}
	return blockers
}

func dependencyBlockersReady(blockers []dependencyBlocker, cfg DependencyAutoUnblockConfig, terminalStates []string) bool {
	if len(blockers) == 0 {
		return false
	}
	for _, blocker := range blockers {
		if !dependencyBlockerReady(blocker, cfg, terminalStates) {
			return false
		}
	}
	return true
}

func dependencyBlockerReady(blocker dependencyBlocker, cfg DependencyAutoUnblockConfig, terminalStates []string) bool {
	if blocker.Resolved {
		if blocker.Issue.Closed || stateIn(blocker.Issue.State, terminalStates) {
			return true
		}
		if cfg.Readiness == DependencyReadinessTerminalOrMerged && pullRequestMerged(blocker.Issue.PullRequest) {
			return true
		}
		return false
	}
	if strings.TrimSpace(blocker.Ref.State) == "" {
		return false
	}
	return stateIn(blocker.Ref.State, terminalStates)
}

func pullRequestMerged(pullRequest *connector.PullRequest) bool {
	return pullRequest != nil && normalizePullRequestState(pullRequest.State) == "merged"
}

func (o *Orchestrator) applyDependencyAutoUnblock(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	blockers []dependencyBlocker,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		if o.logger != nil {
			o.logger.Warn("dependency auto-unblock transition failed", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "error", err)
		}
		return false
	}

	body := dependencyAutoUnblockComment(issue.State, targetState, blockers)
	if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
		o.logger.Warn("dependency auto-unblock comment failed", "issue_id", issueID, "identifier", issue.Identifier, "target_state", targetState, "error", err)
	}

	delete(state.Blocked, issueID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "dependency_auto_unblock_transition",
		Message: "auto-unblocked " + issueLabel(issue) + " from " + issue.State + " to " + targetState,
	})
	if o.logger != nil {
		o.logger.Info("dependency auto-unblock transition", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState)
	}
	return true
}

func dependencyAutoUnblockComment(sourceState string, targetState string, blockers []dependencyBlocker) string {
	var b strings.Builder
	b.WriteString("Dependency blockers cleared.")
	if strings.TrimSpace(sourceState) != "" && strings.TrimSpace(targetState) != "" {
		b.WriteString(" Moved this issue from ")
		b.WriteString(strings.TrimSpace(sourceState))
		b.WriteString(" to ")
		b.WriteString(strings.TrimSpace(targetState))
		b.WriteString(".")
	}
	b.WriteString("\n\nCleared dependencies:")
	for _, blocker := range blockers {
		b.WriteString("\n- ")
		b.WriteString(dependencyBlockerLabel(blocker))
		if state := dependencyBlockerState(blocker); state != "" {
			b.WriteString(" (state: ")
			b.WriteString(state)
			b.WriteString(")")
		}
		if pullRequest := dependencyBlockerPullRequest(blocker); pullRequest != nil && pullRequestMerged(pullRequest) {
			b.WriteString(fmt.Sprintf(" (merged PR: #%d)", pullRequest.Number))
		}
	}
	return b.String()
}

func dependencyBlockerLabel(blocker dependencyBlocker) string {
	if blocker.Resolved {
		if identifier := strings.TrimSpace(blocker.Issue.Identifier); identifier != "" {
			return identifier
		}
		if id := strings.TrimSpace(blocker.Issue.ID); id != "" {
			return id
		}
	}
	if identifier := strings.TrimSpace(blocker.Ref.Identifier); identifier != "" {
		return identifier
	}
	if id := strings.TrimSpace(blocker.Ref.ID); id != "" {
		return id
	}
	return "unknown dependency"
}

func dependencyBlockerState(blocker dependencyBlocker) string {
	if blocker.Resolved {
		return strings.TrimSpace(blocker.Issue.State)
	}
	return strings.TrimSpace(blocker.Ref.State)
}

func dependencyBlockerPullRequest(blocker dependencyBlocker) *connector.PullRequest {
	if !blocker.Resolved {
		return nil
	}
	return blocker.Issue.PullRequest
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func defaultStringSlice(values []string, fallback []string) []string {
	if len(values) == 0 {
		return append([]string(nil), fallback...)
	}
	return append([]string(nil), values...)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
