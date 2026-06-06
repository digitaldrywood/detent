package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

var (
	epicDependencyLinePattern = regexp.MustCompile("(?i)^\\s*(?:>\\s*)?(?:[-*+]\\s+)?(?:[*_`~]+)?\\s*(?:blocked\\s+by|depends[\\s-]+on)(?:[*_`~]+)?\\s*:\\s*(?:[*_`~]+)?\\s*(.+)\\s*$")
	epicChecklistLinePattern  = regexp.MustCompile(`^\s*[-*+]\s+\[[ xX]\]\s+(.+)$`)
	epicIssueRefPattern       = regexp.MustCompile(`(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#(\d+)`)
	epicIssueURLPattern       = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/issues/(\d+)`)
)

type completedEpicPlan struct {
	issue       connector.Issue
	children    []connector.BlockedRef
	retryIssues []connector.Issue
	incomplete  bool
}

type epicIssueIndex struct {
	byID         map[string]connector.Issue
	byIdentifier map[string]connector.Issue
}

func (o *Orchestrator) closeCompletedEpics(ctx context.Context, issues []connector.Issue) map[string]struct{} {
	plans := completedEpicPlans(issues)
	if len(plans) == 0 {
		return nil
	}
	completed, _ := o.closeCompletedEpicPlans(ctx, issues, plans)
	return completed
}

func (o *Orchestrator) closeCompletedEpicPlans(
	ctx context.Context,
	issues []connector.Issue,
	plans []completedEpicPlan,
) (map[string]struct{}, map[string]connector.Issue) {
	if len(plans) == 0 {
		return nil, nil
	}
	index := newEpicIssueIndex(issues)
	o.refreshLinkedEpicChildren(ctx, plans)
	o.resolveMissingEpicChildren(ctx, index, plans)

	completed := map[string]struct{}{}
	failedRefreshes := map[string]connector.Issue{}
	for _, plan := range plans {
		if plan.incomplete {
			for _, issue := range plan.retryIssues {
				key := issueIdentityKey(issue)
				if key != "" {
					failedRefreshes[key] = cloneIssue(issue)
				}
			}
			continue
		}
		if !epicChildrenDone(plan.children, index, o.cfg.TerminalStates) {
			continue
		}
		if strings.TrimSpace(plan.issue.ID) != "" {
			completed[plan.issue.ID] = struct{}{}
		}
		o.finalizeCompletedEpic(ctx, plan.issue, plan.children)
	}
	if len(failedRefreshes) == 0 {
		failedRefreshes = nil
	}
	return completed, failedRefreshes
}

func (o *Orchestrator) closeCompletedEpicsForTerminalTransitions(
	ctx context.Context,
	issues []connector.Issue,
	previous []connector.Issue,
	lastRefreshAt time.Time,
	retryIssues []connector.Issue,
) (map[string]struct{}, map[string]connector.Issue) {
	transitions := terminalTransitionIssues(issues, previous, lastRefreshAt, o.cfg.TerminalStates)
	transitions = mergeIssueSlices(transitions, terminalIssues(retryIssues, o.cfg.TerminalStates))
	if len(transitions) == 0 {
		return nil, nil
	}
	resolver, ok := o.connector.(connector.IssueParentResolver)
	if !ok {
		return nil, nil
	}

	parents := make([]connector.Issue, 0, len(transitions))
	failedTransitions := map[string]connector.Issue{}
	parentTransitions := map[string][]connector.Issue{}
	seenParents := map[string]struct{}{}
	for _, issue := range transitions {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		linkedParents, err := resolver.FetchIssueParents(ctx, issueID)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("fetch completed child epic parents failed", "issue_id", issueID, "error", err)
			}
			key := issueIdentityKey(issue)
			if key != "" {
				failedTransitions[key] = cloneIssue(issue)
			}
			continue
		}
		for _, parent := range linkedParents {
			key := issueIdentityKey(parent)
			if key == "" {
				continue
			}
			parentTransitions[key] = append(parentTransitions[key], cloneIssue(issue))
			if _, ok := seenParents[key]; ok {
				continue
			}
			seenParents[key] = struct{}{}
			parents = append(parents, cloneIssue(parent))
		}
	}
	if len(parents) == 0 {
		return nil, failedTransitions
	}
	plans := completedEpicPlans(parents)
	for index := range plans {
		key := issueIdentityKey(plans[index].issue)
		plans[index].retryIssues = cloneIssues(parentTransitions[key])
	}
	completed, failedRefreshes := o.closeCompletedEpicPlans(ctx, parents, plans)
	return completed, mergeIssueMaps(failedTransitions, failedRefreshes)
}

func (o *Orchestrator) fetchEpicTransitionIssueStates(ctx context.Context, issues []connector.Issue) ([]connector.Issue, bool) {
	ids := issueIDs(issues)
	if len(ids) == 0 {
		return nil, true
	}
	refreshed, err := o.connector.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("fetch epic transition issue states failed", "error", err)
		}
		return nil, false
	}
	return cloneIssues(refreshed), true
}

func (o *Orchestrator) refreshPendingEpicParentLookups(
	ctx context.Context,
	pending map[string]connector.Issue,
) ([]connector.Issue, map[string]connector.Issue) {
	if len(pending) == 0 {
		return nil, nil
	}

	issues := make([]connector.Issue, 0, len(pending))
	for _, key := range sortedKeys(pending) {
		issues = append(issues, pending[key])
	}
	refreshed, ok := o.fetchEpicTransitionIssueStates(ctx, issues)
	if !ok {
		return nil, pending
	}
	return terminalIssues(refreshed, o.cfg.TerminalStates), nil
}

func issueIDs(issues []connector.Issue) []string {
	ids := make([]string, 0, len(issues))
	seen := map[string]struct{}{}
	for _, issue := range issues {
		id := strings.TrimSpace(issue.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func terminalIssues(issues []connector.Issue, terminalStates []string) []connector.Issue {
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if terminalIssue(issue, terminalStates) {
			out = append(out, cloneIssue(issue))
		}
	}
	return out
}

func mergeIssueSlices(left []connector.Issue, right []connector.Issue) []connector.Issue {
	out := make([]connector.Issue, 0, len(left)+len(right))
	seen := map[string]struct{}{}
	for _, issue := range append(cloneIssues(left), right...) {
		key := issueIdentityKey(issue)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, cloneIssue(issue))
	}
	return out
}

func mergeIssueMaps(maps ...map[string]connector.Issue) map[string]connector.Issue {
	out := map[string]connector.Issue{}
	for _, issues := range maps {
		for key, issue := range issues {
			key = strings.TrimSpace(key)
			if key == "" {
				key = issueIdentityKey(issue)
			}
			if key == "" {
				continue
			}
			out[key] = cloneIssue(issue)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func terminalTransitionIssues(
	issues []connector.Issue,
	previous []connector.Issue,
	lastRefreshAt time.Time,
	terminalStates []string,
) []connector.Issue {
	index := newEpicIssueIndex(previous)
	transitions := make([]connector.Issue, 0, len(issues))
	seen := map[string]struct{}{}
	for _, issue := range issues {
		if !terminalIssue(issue, terminalStates) {
			continue
		}
		key := issueIdentityKey(issue)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		if !issueBecameTerminal(issue, index, lastRefreshAt, terminalStates) {
			continue
		}
		seen[key] = struct{}{}
		transitions = append(transitions, cloneIssue(issue))
	}
	return transitions
}

func issueBecameTerminal(
	issue connector.Issue,
	previous *epicIssueIndex,
	lastRefreshAt time.Time,
	terminalStates []string,
) bool {
	if previousIssue, ok := previous.issueForRef(connector.BlockedRef{ID: issue.ID, Identifier: issue.Identifier}); ok {
		return !terminalIssue(previousIssue, terminalStates)
	}
	if lastRefreshAt.IsZero() {
		return false
	}
	if issue.StageUpdatedAt != nil {
		return issue.StageUpdatedAt.After(lastRefreshAt)
	}
	if issue.UpdatedAt != nil {
		return issue.UpdatedAt.After(lastRefreshAt)
	}
	return false
}

func terminalIssue(issue connector.Issue, terminalStates []string) bool {
	return issue.Closed || stateIn(issue.State, terminalStates)
}

func completedEpicPlans(issues []connector.Issue) []completedEpicPlan {
	plans := make([]completedEpicPlan, 0, len(issues))
	for _, issue := range issues {
		if !epicIssue(issue) {
			continue
		}
		children := epicChildRefs(issue)
		if len(children) == 0 {
			continue
		}
		plans = append(plans, completedEpicPlan{
			issue:    cloneIssue(issue),
			children: children,
		})
	}
	return plans
}

func (o *Orchestrator) refreshLinkedEpicChildren(ctx context.Context, plans []completedEpicPlan) {
	resolver, ok := o.connector.(connector.IssueChildrenResolver)
	if !ok {
		return
	}
	for index := range plans {
		issueID := strings.TrimSpace(plans[index].issue.ID)
		if issueID == "" {
			continue
		}
		children, err := resolver.FetchIssueChildren(ctx, issueID)
		if err != nil {
			plans[index].incomplete = true
			if o.logger != nil {
				o.logger.Warn("fetch epic linked children failed", "issue_id", issueID, "error", err)
			}
			continue
		}
		plans[index].children = mergeEpicChildRefs(plans[index].issue, plans[index].children, children)
	}
}

func (o *Orchestrator) resolveMissingEpicChildren(ctx context.Context, index *epicIssueIndex, plans []completedEpicPlan) {
	resolver, ok := o.connector.(connector.IssueReferenceResolver)
	if !ok {
		return
	}

	identifiers := make([]string, 0)
	seen := map[string]struct{}{}
	for _, plan := range plans {
		if plan.incomplete {
			continue
		}
		for _, child := range plan.children {
			if _, ok := index.issueForRef(child); ok {
				continue
			}
			identifier := strings.TrimSpace(child.Identifier)
			if identifier == "" {
				continue
			}
			key := strings.ToLower(identifier)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			identifiers = append(identifiers, identifier)
		}
	}
	if len(identifiers) == 0 {
		return
	}

	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("resolve epic child issues failed", "error", err)
		}
		return
	}
	index.addIssues(issues)
}

func (o *Orchestrator) finalizeCompletedEpic(ctx context.Context, issue connector.Issue, children []connector.BlockedRef) {
	if !stateIn(issue.State, o.cfg.TerminalStates) {
		if err := o.connector.UpdateIssueState(ctx, issue.ID, doneStateName(o.cfg.TerminalStates)); err != nil && o.logger != nil {
			o.logger.Warn("move completed epic to done failed", "issue_id", issue.ID, "error", err)
		}
	}
	if issue.Closed {
		return
	}

	closer, ok := o.connector.(connector.IssueCloser)
	if !ok {
		if o.logger != nil {
			o.logger.Warn("close completed epic unsupported", "issue_id", issue.ID)
		}
		return
	}
	if err := closer.CloseIssue(ctx, issue.ID); err != nil {
		if o.logger != nil {
			o.logger.Warn("close completed epic failed", "issue_id", issue.ID, "error", err)
		}
		return
	}

	body := completedEpicComment(len(children))
	if err := o.connector.CreateComment(ctx, issue.ID, body); err != nil && o.logger != nil {
		o.logger.Warn("comment completed epic failed", "issue_id", issue.ID, "error", err)
	}
}

func completedEpicComment(childCount int) string {
	if childCount == 1 {
		return "Auto-closing completed epic: 1 child issue is Done."
	}
	return fmt.Sprintf("Auto-closing completed epic: %d child issues are Done.", childCount)
}

func epicChildrenDone(children []connector.BlockedRef, index *epicIssueIndex, terminalStates []string) bool {
	for _, child := range children {
		issue, ok := index.issueForRef(child)
		if !ok {
			return false
		}
		if issue.Closed {
			continue
		}
		if !stateIn(issue.State, terminalStates) {
			return false
		}
	}
	return len(children) > 0
}

func newEpicIssueIndex(issues []connector.Issue) *epicIssueIndex {
	index := &epicIssueIndex{
		byID:         make(map[string]connector.Issue, len(issues)),
		byIdentifier: make(map[string]connector.Issue, len(issues)),
	}
	index.addIssues(issues)
	return index
}

func (i *epicIssueIndex) addIssues(issues []connector.Issue) {
	for _, issue := range issues {
		i.addIssue(issue)
	}
}

func (i *epicIssueIndex) addIssue(issue connector.Issue) {
	issue = cloneIssue(issue)
	if id := strings.TrimSpace(issue.ID); id != "" {
		i.byID[id] = issue
	}
	if identifier := normalizedIssueIdentifier(issue.Identifier); identifier != "" {
		i.byIdentifier[identifier] = issue
	}
}

func (i *epicIssueIndex) issueForRef(ref connector.BlockedRef) (connector.Issue, bool) {
	if id := strings.TrimSpace(ref.ID); id != "" {
		if issue, ok := i.byID[id]; ok {
			return cloneIssue(issue), true
		}
	}
	if identifier := normalizedIssueIdentifier(ref.Identifier); identifier != "" {
		if issue, ok := i.byIdentifier[identifier]; ok {
			return cloneIssue(issue), true
		}
	}
	if strings.TrimSpace(ref.State) != "" {
		return connector.Issue{
			ID:         strings.TrimSpace(ref.ID),
			Identifier: strings.TrimSpace(ref.Identifier),
			State:      strings.TrimSpace(ref.State),
		}, true
	}
	return connector.Issue{}, false
}

func filterCompletedEpicCandidates(issues []connector.Issue, completed map[string]struct{}) []connector.Issue {
	if len(completed) == 0 {
		return issues
	}
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := completed[issue.ID]; ok {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func epicIssue(issue connector.Issue) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(label), "epic") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(issue.Title)), "epic:")
}

func epicChildRefs(issue connector.Issue) []connector.BlockedRef {
	linked := append([]connector.BlockedRef(nil), issue.BlockedBy...)
	linked = append(linked, issue.ChildIssues...)
	return mergeEpicChildRefs(issue, parseEpicBodyChildRefs(issue.Description, issueRepo(issue.Identifier)), linked)
}

func mergeEpicChildRefs(issue connector.Issue, groups ...[]connector.BlockedRef) []connector.BlockedRef {
	children := []connector.BlockedRef{}
	positions := map[string]int{}
	selfID := strings.TrimSpace(issue.ID)
	selfIdentifier := normalizedIssueIdentifier(issue.Identifier)

	add := func(ref connector.BlockedRef) {
		ref.ID = strings.TrimSpace(ref.ID)
		ref.Identifier = strings.TrimSpace(ref.Identifier)
		ref.State = strings.TrimSpace(ref.State)
		if ref.ID == "" && ref.Identifier == "" {
			return
		}
		if ref.ID != "" && ref.ID == selfID {
			return
		}
		if normalizedIssueIdentifier(ref.Identifier) == selfIdentifier {
			return
		}
		key := epicRefKey(ref)
		if index, ok := positions[key]; ok {
			children[index] = mergeEpicRef(children[index], ref)
			return
		}
		positions[key] = len(children)
		children = append(children, ref)
	}

	for _, group := range groups {
		for _, ref := range group {
			if strings.TrimSpace(ref.Identifier) == "" && strings.TrimSpace(ref.ID) == "" {
				continue
			}
			add(ref)
		}
	}
	return children
}

func parseEpicBodyChildRefs(body string, repo string) []connector.BlockedRef {
	children := []connector.BlockedRef{}
	for _, line := range strings.FieldsFunc(body, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		if matches := epicDependencyLinePattern.FindStringSubmatch(line); len(matches) == 2 {
			children = append(children, parseEpicRefs(matches[1], repo)...)
			continue
		}
		if matches := epicChecklistLinePattern.FindStringSubmatch(line); len(matches) == 2 {
			children = append(children, parseEpicRefs(matches[1], repo)...)
		}
	}
	return children
}

func parseEpicRefs(text string, repo string) []connector.BlockedRef {
	refs := []connector.BlockedRef{}
	seen := map[string]struct{}{}
	addIdentifier := func(identifier string) {
		identifier = strings.TrimSpace(identifier)
		key := normalizedIssueIdentifier(identifier)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, connector.BlockedRef{Identifier: identifier})
	}

	for _, matches := range epicIssueURLPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			addIdentifier(blockerIdentifier(matches[1], matches[2], repo))
		}
	}
	for _, matches := range epicIssueRefPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			addIdentifier(blockerIdentifier(matches[1], matches[2], repo))
		}
	}
	return refs
}

func mergeEpicRef(current connector.BlockedRef, incoming connector.BlockedRef) connector.BlockedRef {
	if current.ID == "" {
		current.ID = incoming.ID
	}
	if current.Identifier == "" {
		current.Identifier = incoming.Identifier
	}
	if current.State == "" {
		current.State = incoming.State
	}
	return current
}

func epicRefKey(ref connector.BlockedRef) string {
	if identifier := normalizedIssueIdentifier(ref.Identifier); identifier != "" {
		return identifier
	}
	return "id:" + strings.TrimSpace(ref.ID)
}

func issueRepo(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 {
		return ""
	}
	return strings.TrimSpace(identifier[:index])
}

func blockerIdentifier(refRepo string, number string, repo string) string {
	if strings.TrimSpace(number) == "" {
		return ""
	}
	refRepo = strings.TrimSpace(refRepo)
	if refRepo == "" {
		if strings.TrimSpace(repo) == "" {
			return "#" + number
		}
		refRepo = strings.TrimSpace(repo)
	}
	return refRepo + "#" + number
}

func normalizedIssueIdentifier(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func issueIdentityKey(issue connector.Issue) string {
	if id := strings.TrimSpace(issue.ID); id != "" {
		return "id:" + id
	}
	if identifier := normalizedIssueIdentifier(issue.Identifier); identifier != "" {
		return "identifier:" + identifier
	}
	return ""
}

func doneStateName(terminalStates []string) string {
	for _, state := range terminalStates {
		if normalizeState(state) == "done" {
			return "Done"
		}
	}
	if len(terminalStates) > 0 {
		return strings.TrimSpace(terminalStates[0])
	}
	return "Done"
}
