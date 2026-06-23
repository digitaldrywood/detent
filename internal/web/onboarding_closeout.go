package web

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const onboardingCloseoutWait = 1500 * time.Millisecond

type onboardingCloseoutReport struct {
	ok      bool
	details []string
}

func (s *Server) onboardingCloseoutSnapshot() (telemetry.Snapshot, bool) {
	if s.hub == nil {
		return telemetry.Snapshot{}, false
	}
	return s.hub.Latest()
}

func (s *Server) runOnboardingCloseout(
	ctx context.Context,
	form templates.OnboardingForm,
	before telemetry.Snapshot,
	beforeOK bool,
) onboardingCloseoutReport {
	report := onboardingCloseoutReport{
		ok:      true,
		details: []string{"Closeout verifier"},
	}
	if s.hub == nil || s.refresher == nil {
		report.ok = false
		report.details = append(report.details,
			"reload: stalled (live runtime snapshot or refresher is unavailable)",
			"refresh: stalled (cannot request /api/v1/refresh from this onboarding context)",
			onboardingActiveAgentInventory(before),
			"restart requires operator confirmation after reviewing active agents",
		)
		return report
	}

	projectID := s.onboardingCloseoutProjectID(before, beforeOK)
	beforeRunning := onboardingRunningIDs(before.Running)
	sub, err := s.hub.Subscribe(ctx)
	if err != nil {
		sub = nil
	}
	if sub != nil {
		defer sub.Close()
	}

	response, err := s.refresher.RequestRefresh(ctx)
	if err != nil {
		after, ok := s.onboardingCloseoutSnapshot()
		if !ok {
			after = before
		}
		report.ok = false
		report.details = append(report.details,
			"reload: stalled (refresh request failed)",
			"refresh: stalled ("+err.Error()+")",
			onboardingDispatchDetail(beforeRunning, projectID, before, after),
			onboardingActiveAgentInventory(after),
			"restart requires operator confirmation after reviewing active agents",
		)
		return report
	}

	after := s.waitOnboardingCloseoutSnapshot(ctx, sub, before, projectID, beforeRunning, response.RequestedAt)
	if projectID == "" {
		projectID = s.onboardingCloseoutProjectID(after, true)
	}
	if !onboardingReloadObserved(before, beforeOK, after, projectID, response.RequestedAt) {
		report.ok = false
		report.details = append(report.details, "reload: stalled")
	} else {
		report.details = append(report.details, "reload: observed project "+projectID)
	}

	if !onboardingRefreshAdvanced(before, beforeOK, after, projectID, response.RequestedAt) {
		report.ok = false
		report.details = append(report.details, "refresh: stalled")
	} else {
		report.details = append(report.details, "refresh: advanced")
	}

	if detail, ok := onboardingCandidateCountsDetail(form, after, projectID); detail != "" {
		if !ok {
			report.ok = false
		}
		report.details = append(report.details, detail)
	}

	dispatchDetails, dispatchOK := onboardingDispatchDetails(beforeRunning, projectID, before, after)
	if !dispatchOK {
		report.ok = false
	}
	report.details = append(report.details, dispatchDetails...)

	if onboardingRefreshReflected(after, projectID, response.RequestedAt) {
		report.details = append(report.details, "/api/v1/refresh: reflected in project state")
	} else {
		report.ok = false
		report.details = append(report.details, "/api/v1/refresh: not reflected in project state")
	}

	if !report.ok {
		report.details = append(report.details,
			onboardingActiveAgentInventory(after),
			"restart requires operator confirmation after reviewing active agents",
		)
	}
	return report
}

func (s *Server) waitOnboardingCloseoutSnapshot(
	ctx context.Context,
	sub interface {
		C() <-chan telemetry.Snapshot
	},
	before telemetry.Snapshot,
	projectID string,
	beforeRunning map[string]struct{},
	requestedAt time.Time,
) telemetry.Snapshot {
	if after, ok := s.onboardingCloseoutSnapshot(); ok &&
		onboardingCloseoutObserved(before, after, projectID, beforeRunning, requestedAt) {
		return after
	}
	if sub == nil {
		if after, ok := s.onboardingCloseoutSnapshot(); ok {
			return after
		}
		return before
	}

	timer := time.NewTimer(onboardingCloseoutWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if after, ok := s.onboardingCloseoutSnapshot(); ok {
				return after
			}
			return before
		case after, ok := <-sub.C():
			if !ok {
				if latest, latestOK := s.onboardingCloseoutSnapshot(); latestOK {
					return latest
				}
				return before
			}
			if onboardingCloseoutObserved(before, after, projectID, beforeRunning, requestedAt) {
				return after
			}
		case <-timer.C:
			if after, ok := s.onboardingCloseoutSnapshot(); ok {
				return after
			}
			return before
		}
	}
}

func onboardingCloseoutObserved(
	before telemetry.Snapshot,
	after telemetry.Snapshot,
	projectID string,
	beforeRunning map[string]struct{},
	requestedAt time.Time,
) bool {
	if onboardingRefreshAdvanced(before, true, after, projectID, requestedAt) {
		return true
	}
	return len(onboardingNewRunning(beforeRunning, projectID, after.Running)) > 0
}

func (s *Server) onboardingCloseoutProjectID(snapshot telemetry.Snapshot, snapshotOK bool) string {
	if id := s.onboardingCloseoutProjectIDFromRegistry(); id != "" {
		return id
	}
	if id := onboardingCloseoutProjectIDFromConfig(s.globalConfigSource, s.globalConfig, s.workflow); id != "" {
		return id
	}
	if !snapshotOK {
		return ""
	}
	if len(snapshot.Projects) == 1 {
		return projectID(snapshot.Projects[0].Project)
	}
	if id := projectID(snapshot.Project); id != "" && !strings.EqualFold(id, "multiple projects") {
		return id
	}
	return ""
}

func (s *Server) onboardingCloseoutProjectIDFromRegistry() string {
	if s.registry == nil {
		return ""
	}
	for _, trackedProject := range s.registry.List() {
		if trackedProject == nil {
			continue
		}
		if sameOnboardingWorkflowPath(trackedProject.Config().Workflow, s.workflow) {
			return strings.TrimSpace(string(trackedProject.ID()))
		}
	}
	return ""
}

func onboardingCloseoutProjectIDFromConfig(
	source func() globalconfig.Config,
	fallback globalconfig.Config,
	workflowPath string,
) string {
	cfg := fallback
	if source != nil {
		cfg = source()
	}
	for _, project := range cfg.Projects {
		if sameOnboardingWorkflowPath(project.Workflow, workflowPath) {
			return strings.TrimSpace(project.ID)
		}
	}
	return ""
}

func sameOnboardingWorkflowPath(left string, right string) bool {
	left = cleanOnboardingWorkflowPath(left)
	right = cleanOnboardingWorkflowPath(right)
	return left != "" && right != "" && left == right
}

func cleanOnboardingWorkflowPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func onboardingReloadObserved(
	before telemetry.Snapshot,
	beforeOK bool,
	after telemetry.Snapshot,
	projectID string,
	requestedAt time.Time,
) bool {
	if projectID == "" {
		return false
	}
	if _, ok := onboardingProjectRefresh(after, projectID); !ok {
		return false
	}
	if !beforeOK {
		return true
	}
	return onboardingRefreshAdvanced(before, beforeOK, after, projectID, requestedAt)
}

func onboardingRefreshAdvanced(
	before telemetry.Snapshot,
	beforeOK bool,
	after telemetry.Snapshot,
	projectID string,
	requestedAt time.Time,
) bool {
	afterRefresh, ok := onboardingProjectRefresh(after, projectID)
	if !ok || afterRefresh.LastRefreshAt == nil {
		return false
	}
	if !requestedAt.IsZero() && afterRefresh.LastRefreshAt.Before(requestedAt) {
		return false
	}
	if !beforeOK {
		return true
	}
	beforeRefresh, ok := onboardingProjectRefresh(before, projectID)
	if !ok || beforeRefresh.LastRefreshAt == nil {
		return true
	}
	return afterRefresh.LastRefreshAt.After(*beforeRefresh.LastRefreshAt)
}

func onboardingRefreshReflected(snapshot telemetry.Snapshot, projectID string, requestedAt time.Time) bool {
	if requestedAt.IsZero() {
		return true
	}
	refresh, ok := onboardingProjectRefresh(snapshot, projectID)
	return ok && refresh.LastRefreshAt != nil && !refresh.LastRefreshAt.Before(requestedAt)
}

func onboardingProjectRefresh(snapshot telemetry.Snapshot, projectID string) (telemetry.Refresh, bool) {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" {
		if project, ok := projectSnapshotForID(snapshot, projectID); ok {
			if projectSnapshotHasRefreshSignal(project) {
				return project.Refresh, true
			}
		}
		if telemetryProjectMatches(snapshot.Project, projectID) && refreshHasSignal(snapshot.Refresh) {
			return snapshot.Refresh, true
		}
	}
	if refreshHasSignal(snapshot.Refresh) {
		return snapshot.Refresh, true
	}
	return telemetry.Refresh{}, false
}

func refreshHasSignal(refresh telemetry.Refresh) bool {
	return refresh.PollIntervalSeconds != 0 ||
		refresh.Status != "" ||
		refresh.LastRefreshAt != nil ||
		refresh.NextRefreshAt != nil ||
		strings.TrimSpace(refresh.LastError) != "" ||
		refresh.LastErrorAt != nil
}

func onboardingCandidateCountsDetail(
	form templates.OnboardingForm,
	snapshot telemetry.Snapshot,
	projectID string,
) (string, bool) {
	if normalizedOnboardingGitHubStatusSource(form.GitHubStatusSource) != workflowconfig.GitHubStatusSourceLabel {
		return "", true
	}
	prefix := strings.TrimSpace(form.StatusLabelPrefix)
	if prefix == "" {
		prefix = defaultStatusLabelPrefix
	}
	issues := onboardingProjectIssues(snapshot, projectID)
	if len(issues) == 0 {
		return "candidate counts: no issues observed for expected status labels", false
	}
	mismatches := []string{}
	matches := 0
	for _, issue := range issues {
		label := onboardingStatusLabel(prefix, issue.State)
		if label == "" {
			continue
		}
		if onboardingIssueHasLabel(issue, label) {
			matches++
			continue
		}
		if onboardingIssueHasStatusPrefix(issue, prefix) {
			mismatches = append(mismatches, onboardingIssueLabel(issue)+" expected "+label)
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		return "candidate counts: status label mismatch (" + strings.Join(mismatches, "; ") + ")", false
	}
	if matches == 0 {
		return "candidate counts: no issues observed for expected status labels", false
	}
	return "candidate counts: expected status labels matched", true
}

func onboardingProjectIssues(snapshot telemetry.Snapshot, projectID string) []telemetry.Issue {
	issues := make([]telemetry.Issue, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked))
	for _, issue := range snapshot.BoardIssues {
		if onboardingIssueMatchesProject(issue, projectID) {
			issues = append(issues, issue)
		}
	}
	for _, issue := range snapshot.Pipeline {
		if onboardingIssueMatchesProject(issue, projectID) {
			issues = append(issues, issue)
		}
	}
	for _, running := range snapshot.Running {
		if onboardingIssueMatchesProject(running.Issue, projectID) {
			issues = append(issues, running.Issue)
		}
	}
	for _, queued := range snapshot.Queue {
		if onboardingIssueMatchesProject(queued.Issue, projectID) {
			issues = append(issues, queued.Issue)
		}
	}
	for _, blocked := range snapshot.Blocked {
		if onboardingIssueMatchesProject(blocked.Issue, projectID) {
			issues = append(issues, blocked.Issue)
		}
	}
	return issues
}

func onboardingIssueMatchesProject(issue telemetry.Issue, projectID string) bool {
	projectID = strings.TrimSpace(projectID)
	return projectID == "" || strings.TrimSpace(issue.ProjectID) == "" || strings.TrimSpace(issue.ProjectID) == projectID
}

func onboardingIssueHasLabel(issue telemetry.Issue, label string) bool {
	for _, candidate := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(candidate), label) {
			return true
		}
	}
	return false
}

func onboardingIssueHasStatusPrefix(issue telemetry.Issue, prefix string) bool {
	for _, label := range issue.Labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label)), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

func onboardingStatusLabel(prefix string, state string) string {
	slug := onboardingStatusSlug(state)
	if slug == "" {
		return ""
	}
	return prefix + slug
}

func onboardingStatusSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastSeparator := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSeparator = false
		default:
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteByte('-')
			lastSeparator = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func onboardingRunningIDs(entries []telemetry.Running) map[string]struct{} {
	out := map[string]struct{}{}
	for _, entry := range entries {
		key := onboardingRunningKey(entry)
		if key != "" {
			out[key] = struct{}{}
		}
	}
	return out
}

func onboardingNewRunning(before map[string]struct{}, projectID string, after []telemetry.Running) []telemetry.Running {
	out := []telemetry.Running{}
	for _, entry := range after {
		if !onboardingIssueMatchesProject(entry.Issue, projectID) {
			continue
		}
		key := onboardingRunningKey(entry)
		if key == "" {
			continue
		}
		if _, ok := before[key]; ok {
			continue
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return onboardingIssueLabel(out[i].Issue) < onboardingIssueLabel(out[j].Issue)
	})
	return out
}

func onboardingRunningKey(entry telemetry.Running) string {
	if id := strings.TrimSpace(entry.ID); id != "" {
		return id
	}
	if id := strings.TrimSpace(entry.Identifier); id != "" {
		return id
	}
	return strings.TrimSpace(entry.SessionID)
}

func onboardingDispatchDetail(
	beforeRunning map[string]struct{},
	projectID string,
	before telemetry.Snapshot,
	after telemetry.Snapshot,
) string {
	details, _ := onboardingDispatchDetails(beforeRunning, projectID, before, after)
	if len(details) == 0 {
		return "dispatch: no new running issue observed"
	}
	return details[0]
}

func onboardingDispatchDetails(
	beforeRunning map[string]struct{},
	projectID string,
	before telemetry.Snapshot,
	after telemetry.Snapshot,
) ([]string, bool) {
	started := onboardingNewRunning(beforeRunning, projectID, after.Running)
	if len(started) == 0 {
		return []string{onboardingDispatchStallReason(before, after, projectID)}, false
	}
	details := []string{}
	ok := true
	for _, entry := range started {
		label := onboardingIssueLabel(entry.Issue)
		details = append(details, "dispatch: started "+label)
		if strings.TrimSpace(entry.WorkspacePath) == "" {
			ok = false
			details = append(details, "worktree: missing for "+label)
		} else {
			details = append(details, "worktree: present for "+label)
		}
		if onboardingIssueHasWorkpad(entry.Issue) {
			details = append(details, "Workpad: present for "+label)
		} else {
			ok = false
			details = append(details, "Workpad: missing for "+label)
		}
	}
	return details, ok
}

func onboardingDispatchStallReason(before telemetry.Snapshot, after telemetry.Snapshot, projectID string) string {
	if after.Shutdown.Draining {
		return "dispatch: no new running issue observed (runtime is draining)"
	}
	if len(onboardingProjectIssues(after, projectID)) == 0 {
		return "dispatch: no new running issue observed (no candidate issues in project state)"
	}
	if after.Counts.Queue > before.Counts.Queue {
		return "dispatch: no new running issue observed (issues queued for retry)"
	}
	if after.Counts.Blocked > before.Counts.Blocked {
		return "dispatch: no new running issue observed (issues blocked)"
	}
	return "dispatch: no new running issue observed"
}

func onboardingIssueHasWorkpad(issue telemetry.Issue) bool {
	for _, comment := range issue.Comments {
		if strings.Contains(comment.Body, "## Codex Workpad") {
			return true
		}
	}
	return false
}

func onboardingIssueLabel(issue telemetry.Issue) string {
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return identifier
	}
	if id := strings.TrimSpace(issue.ID); id != "" {
		return id
	}
	return "unknown issue"
}

func onboardingActiveAgentInventory(snapshot telemetry.Snapshot) string {
	if len(snapshot.Running) == 0 {
		return "active agents: none observed"
	}
	entries := make([]string, 0, len(snapshot.Running))
	for _, running := range snapshot.Running {
		entries = append(entries, fmt.Sprintf("%s session=%s pid=%s worker=%s worktree=%s",
			onboardingIssueLabel(running.Issue),
			onboardingInventoryValue(running.SessionID),
			onboardingInventoryValue(running.ProcessIdentity),
			onboardingInventoryValue(running.WorkerHost),
			onboardingInventoryValue(running.WorkspacePath),
		))
	}
	sort.Strings(entries)
	return "active agents: " + strings.Join(entries, "; ")
}

func onboardingInventoryValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}
