package kanban

import (
	"maps"
	"sort"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type SnapshotIssueEntry struct {
	Issue telemetry.Issue
	State string

	rank            int
	index           int
	rawRuntimeState bool
}

func FilterSnapshotIssues(snapshot *telemetry.Snapshot, keep func(telemetry.Issue) bool) {
	if snapshot == nil || keep == nil {
		return
	}
	snapshot.BoardIssues = filterIssues(snapshot.BoardIssues, keep)
	snapshot.Pipeline = filterIssues(snapshot.Pipeline, keep)
	snapshot.Running = filterRunning(snapshot.Running, keep)
	snapshot.Queue = filterQueued(snapshot.Queue, keep)
	snapshot.Blocked = filterBlocked(snapshot.Blocked, keep)
}

func filterIssues(issues []telemetry.Issue, keep func(telemetry.Issue) bool) []telemetry.Issue {
	out := issues[:0]
	for _, issue := range issues {
		if keep(issue) {
			out = append(out, issue)
		}
	}
	return out
}

func filterRunning(rows []telemetry.Running, keep func(telemetry.Issue) bool) []telemetry.Running {
	out := rows[:0]
	for _, row := range rows {
		if keep(row.Issue) {
			out = append(out, row)
		}
	}
	return out
}

func filterQueued(rows []telemetry.Queued, keep func(telemetry.Issue) bool) []telemetry.Queued {
	out := rows[:0]
	for _, row := range rows {
		if keep(row.Issue) {
			out = append(out, row)
		}
	}
	return out
}

func filterBlocked(rows []telemetry.Blocked, keep func(telemetry.Issue) bool) []telemetry.Blocked {
	out := rows[:0]
	for _, row := range rows {
		if keep(row.Issue) {
			out = append(out, row)
		}
	}
	return out
}

func CloneIssueSlices(snapshot telemetry.Snapshot) telemetry.Snapshot {
	snapshot.BoardIssues = append([]telemetry.Issue(nil), snapshot.BoardIssues...)
	for i := range snapshot.BoardIssues {
		snapshot.BoardIssues[i] = CloneIssue(snapshot.BoardIssues[i])
	}
	snapshot.Pipeline = append([]telemetry.Issue(nil), snapshot.Pipeline...)
	for i := range snapshot.Pipeline {
		snapshot.Pipeline[i] = CloneIssue(snapshot.Pipeline[i])
	}
	snapshot.Running = append([]telemetry.Running(nil), snapshot.Running...)
	for i := range snapshot.Running {
		snapshot.Running[i].Issue = CloneIssue(snapshot.Running[i].Issue)
	}
	snapshot.Queue = append([]telemetry.Queued(nil), snapshot.Queue...)
	for i := range snapshot.Queue {
		snapshot.Queue[i].Issue = CloneIssue(snapshot.Queue[i].Issue)
	}
	snapshot.Blocked = append([]telemetry.Blocked(nil), snapshot.Blocked...)
	for i := range snapshot.Blocked {
		snapshot.Blocked[i].Issue = CloneIssue(snapshot.Blocked[i].Issue)
	}
	snapshot.Completed = append([]telemetry.Completed(nil), snapshot.Completed...)
	for i := range snapshot.Completed {
		snapshot.Completed[i].Issue = CloneIssue(snapshot.Completed[i].Issue)
	}
	return snapshot
}

func CloneIssue(issue telemetry.Issue) telemetry.Issue {
	out := issue
	if issue.Priority != nil {
		priority := *issue.Priority
		out.Priority = &priority
	}
	out.Labels = append([]string(nil), issue.Labels...)
	out.Assignees = append([]string(nil), issue.Assignees...)
	out.Comments = cloneIssueComments(issue.Comments)
	out.BlockedBy = append([]telemetry.BlockedRef(nil), issue.BlockedBy...)
	out.Metadata = maps.Clone(issue.Metadata)
	out.PullRequest = clonePullRequest(issue.PullRequest)
	out.Deliverable = cloneDeliverable(issue.Deliverable)
	out.MergeTiming = cloneMergeTiming(issue.MergeTiming)
	out.LeaseRenewedAt = CloneTimePointer(issue.LeaseRenewedAt)
	out.LeaseExpiresAt = CloneTimePointer(issue.LeaseExpiresAt)
	out.CreatedAt = CloneTimePointer(issue.CreatedAt)
	out.UpdatedAt = CloneTimePointer(issue.UpdatedAt)
	out.StageUpdatedAt = CloneTimePointer(issue.StageUpdatedAt)
	out.CurrentLaneEnteredAt = CloneTimePointer(issue.CurrentLaneEnteredAt)
	return out
}

func cloneIssueComments(comments []telemetry.IssueComment) []telemetry.IssueComment {
	if comments == nil {
		return nil
	}
	out := make([]telemetry.IssueComment, len(comments))
	for index, comment := range comments {
		out[index] = comment
		out[index].CreatedAt = CloneTimePointer(comment.CreatedAt)
		out[index].UpdatedAt = CloneTimePointer(comment.UpdatedAt)
	}
	return out
}

func clonePullRequest(pr *telemetry.PullRequest) *telemetry.PullRequest {
	if pr == nil {
		return nil
	}
	out := *pr
	if pr.MergeQueueEntry != nil {
		entry := *pr.MergeQueueEntry
		entry.EnqueuedAt = CloneTimePointer(pr.MergeQueueEntry.EnqueuedAt)
		out.MergeQueueEntry = &entry
	}
	out.HydrationNextRetryAt = CloneTimePointer(pr.HydrationNextRetryAt)
	out.SlowChecks = append([]telemetry.PullRequestCheck(nil), pr.SlowChecks...)
	out.RunningChecks = append([]string(nil), pr.RunningChecks...)
	out.RequiredCheckFailures = append([]telemetry.PullRequestCheck(nil), pr.RequiredCheckFailures...)
	return &out
}

func cloneDeliverable(deliverable *telemetry.Deliverable) *telemetry.Deliverable {
	if deliverable == nil {
		return nil
	}
	out := *deliverable
	out.Metadata = maps.Clone(deliverable.Metadata)
	return &out
}

func cloneMergeTiming(timing *telemetry.MergeTiming) *telemetry.MergeTiming {
	if timing == nil {
		return nil
	}
	out := *timing
	out.EnteredMergingAt = CloneTimePointer(timing.EnteredMergingAt)
	out.MergeWorkerSlotAcquiredAt = CloneTimePointer(timing.MergeWorkerSlotAcquiredAt)
	out.MergeStartedAt = CloneTimePointer(timing.MergeStartedAt)
	out.BaseRefreshStartedAt = CloneTimePointer(timing.BaseRefreshStartedAt)
	out.BaseRefreshFinishedAt = CloneTimePointer(timing.BaseRefreshFinishedAt)
	out.CIWaitStartedAt = CloneTimePointer(timing.CIWaitStartedAt)
	out.CIWaitFinishedAt = CloneTimePointer(timing.CIWaitFinishedAt)
	out.MergedAt = CloneTimePointer(timing.MergedAt)
	out.MergeFailedAt = CloneTimePointer(timing.MergeFailedAt)
	return &out
}

func CloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func ApplySnapshotIssues(snapshot *telemetry.Snapshot, apply func(*telemetry.Issue)) {
	if snapshot == nil || apply == nil {
		return
	}
	for i := range snapshot.BoardIssues {
		apply(&snapshot.BoardIssues[i])
	}
	for i := range snapshot.Pipeline {
		apply(&snapshot.Pipeline[i])
	}
	for i := range snapshot.Running {
		apply(&snapshot.Running[i].Issue)
	}
	for i := range snapshot.Queue {
		apply(&snapshot.Queue[i].Issue)
	}
	for i := range snapshot.Blocked {
		apply(&snapshot.Blocked[i].Issue)
	}
	for i := range snapshot.Completed {
		apply(&snapshot.Completed[i].Issue)
	}
}

func IssueStateIndex(snapshot telemetry.Snapshot) map[string]string {
	states := map[string]string{}
	addIssue := func(issue telemetry.Issue, state string) {
		state = strings.TrimSpace(state)
		if state == "" {
			state = strings.TrimSpace(issue.State)
		}
		if state == "" {
			return
		}
		for _, key := range issueStateKeys(issue.ID, issue.Identifier) {
			states[key] = state
		}
	}
	for _, row := range snapshot.Completed {
		addIssue(row.Issue, row.State)
	}
	for _, issue := range SnapshotIssues(snapshot) {
		addIssue(issue, issue.State)
	}
	return states
}

func BlockedRefsWithCurrentStates(refs []telemetry.BlockedRef, states map[string]string) []telemetry.BlockedRef {
	if len(refs) == 0 {
		return refs
	}
	out := append([]telemetry.BlockedRef(nil), refs...)
	for i := range out {
		for _, key := range issueStateKeys(out[i].ID, out[i].Identifier) {
			if state := strings.TrimSpace(states[key]); state != "" {
				out[i].State = state
				break
			}
		}
	}
	return out
}

func issueStateKeys(id string, identifier string) []string {
	keys := []string{}
	if id = strings.TrimSpace(id); id != "" {
		keys = append(keys, "id:"+id)
	}
	if identifier = strings.ToLower(strings.TrimSpace(identifier)); identifier != "" {
		keys = append(keys, "identifier:"+identifier)
	}
	return keys
}

func CardFreshEntry(snapshot telemetry.Snapshot, projectID string, issueID string) (SnapshotIssueEntry, bool) {
	var selected SnapshotIssueEntry
	for _, entry := range visibleSnapshotIssueEntries(snapshot) {
		if !SameIssue(entry.Issue, projectID, issueID, snapshot.Project.ID) {
			continue
		}
		if selected.Issue.ID != "" && entry.rank < selected.rank {
			continue
		}
		selected = entry
	}
	return selected, selected.Issue.ID != ""
}

func SnapshotProjectDataSeq(snapshot telemetry.Snapshot, projectID string) uint64 {
	for _, project := range snapshot.Projects {
		if project.Project.ID == projectID {
			return project.Refresh.DataSeq
		}
	}
	return snapshot.Refresh.DataSeq
}

func StateAllowed(cfg workflowconfig.Config, state string) bool {
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	states := StateNames(cfg, telemetry.Snapshot{})
	if len(states) == 0 {
		return true
	}
	for _, configured := range states {
		if NormalizeState(configured) == NormalizeState(state) {
			return true
		}
	}
	return false
}

func StateNames(cfg workflowconfig.Config, snapshot telemetry.Snapshot) []string {
	states := make([]string, 0, len(cfg.Tracker.ActiveStates)+len(cfg.Tracker.ObservedStates)+len(cfg.Tracker.TerminalStates))
	seen := map[string]struct{}{}
	add := func(values ...string) {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := NormalizeState(value)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			states = append(states, value)
		}
	}
	add(cfg.KanbanStateNames()...)
	for _, issue := range SnapshotIssues(snapshot) {
		if rawGitHubIssueState(issue.State) {
			if _, ok := seen[NormalizeState(issue.State)]; !ok {
				continue
			}
		}
		add(issue.State)
	}
	return states
}

func rawGitHubIssueState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "open", "closed":
		return true
	default:
		return false
	}
}

func AllowedTransitions(cfg workflowconfig.Config, states []string) map[string][]string {
	out := make(map[string][]string, len(states))
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		out[state] = cfg.KanbanAllowedTransitionTargets(state)
	}
	return out
}

func SnapshotIssues(snapshot telemetry.Snapshot) []telemetry.Issue {
	issues := make([]telemetry.Issue, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked))
	issues = append(issues, snapshot.BoardIssues...)
	issues = append(issues, snapshot.Pipeline...)
	for _, row := range snapshot.Running {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Queue {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Blocked {
		issues = append(issues, row.Issue)
	}
	return issues
}

func snapshotIssueEntries(snapshot telemetry.Snapshot) []SnapshotIssueEntry {
	entries := make([]SnapshotIssueEntry, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked))
	index := 0
	appendIssue := func(issue telemetry.Issue, fallback string, rank int) {
		state := strings.TrimSpace(issue.State)
		fallback = strings.TrimSpace(fallback)
		rawRuntimeState := false
		if fallback != "" && rawGitHubIssueState(state) {
			state = fallback
			rawRuntimeState = true
		}
		if state == "" {
			state = fallback
		}
		entries = append(entries, SnapshotIssueEntry{
			Issue:           issue,
			State:           state,
			rank:            rank,
			index:           index,
			rawRuntimeState: rawRuntimeState,
		})
		index++
	}
	for _, issue := range snapshot.BoardIssues {
		appendIssue(issue, "", 5)
	}
	for _, issue := range snapshot.Pipeline {
		appendIssue(issue, "", 10)
	}
	for _, row := range snapshot.Queue {
		appendIssue(row.Issue, "Todo", 20)
	}
	for _, row := range snapshot.Running {
		appendIssue(row.Issue, "In Progress", 30)
	}
	for _, row := range snapshot.Blocked {
		appendIssue(row.Issue, "Blocked", 40)
	}
	return entries
}

func visibleSnapshotIssueEntries(snapshot telemetry.Snapshot) []SnapshotIssueEntry {
	entries := snapshotIssueEntries(snapshot)
	byKey := make(map[string]SnapshotIssueEntry, len(entries))
	for _, entry := range entries {
		key := snapshotIssueEntryKey(entry.Issue, snapshot.Project.ID)
		if key == "" {
			continue
		}
		current, ok := byKey[key]
		if ok && entry.rawRuntimeState {
			continue
		}
		if ok && entry.rank < current.rank {
			continue
		}
		byKey[key] = entry
	}
	visible := make([]SnapshotIssueEntry, 0, len(byKey))
	for _, entry := range byKey {
		visible = append(visible, entry)
	}
	sort.SliceStable(visible, func(i, j int) bool {
		return visible[i].index < visible[j].index
	})
	return visible
}

func snapshotIssueEntryKey(issue telemetry.Issue, snapshotProjectID string) string {
	projectID := strings.TrimSpace(issue.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(snapshotProjectID)
	}
	prefix := ""
	if projectID != "" {
		prefix = "project:" + projectID + ":"
	}
	if issueID := strings.TrimSpace(issue.ID); issueID != "" {
		return prefix + "id:" + issueID
	}
	if identifier := strings.ToLower(strings.TrimSpace(issue.Identifier)); identifier != "" {
		return prefix + "identifier:" + identifier
	}
	return ""
}

func SameIssue(issue telemetry.Issue, projectID string, issueID string, snapshotProjectID string) bool {
	if strings.TrimSpace(issue.ID) != strings.TrimSpace(issueID) {
		return false
	}
	return SameProject(issue, projectID, snapshotProjectID)
}

func SameProject(issue telemetry.Issue, projectID string, snapshotProjectID string) bool {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return true
	}
	issueProjectID := strings.TrimSpace(issue.ProjectID)
	if issueProjectID != "" {
		return issueProjectID == projectID
	}
	return strings.TrimSpace(snapshotProjectID) == "" || strings.TrimSpace(snapshotProjectID) == projectID
}

func IssueRepository(identifier string) string {
	repo, _, ok := strings.Cut(strings.TrimSpace(identifier), "#")
	if !ok {
		return ""
	}
	return strings.TrimSpace(repo)
}

func MappedState(cfg workflowconfig.Config, state string) string {
	state = strings.TrimSpace(state)
	if !cfg.Tracker.StateMap.IsMap {
		return state
	}
	if mapped, ok := cfg.Tracker.StateMap.Map[state]; ok {
		if value, ok := mapped.(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	normalized := NormalizeState(state)
	for detentState, mapped := range cfg.Tracker.StateMap.Map {
		if NormalizeState(detentState) != normalized {
			continue
		}
		if value, ok := mapped.(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return state
}

func NormalizeState(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}
