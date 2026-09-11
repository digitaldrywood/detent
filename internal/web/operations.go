package web

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operations"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) operationsReport(c echo.Context) (operations.Report, error) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.After(now) {
			return operations.Report{}, echo.NewHTTPError(http.StatusBadRequest, "since must be an RFC3339 timestamp at or before now")
		}
		since = parsed
	}
	report, err := s.store.OperationsReport(c.Request().Context(), now, since)
	if err != nil {
		return operations.Report{}, err
	}
	report.Instance = s.instanceName()
	snapshot := s.latestSnapshot(c.Request().Context())

	issues := dailyDigestSnapshotIssues(snapshot)
	mergeDepths := map[string]int{}
	seen := map[string]bool{}
	currentIssues := map[string]telemetry.Issue{}
	for _, issue := range issues {
		projectID := operationsProjectScope(issue.ProjectID, snapshot.Project.ID)
		for _, ref := range []string{issue.Identifier, issue.ID} {
			if key := operationsProjectIssueKey(projectID, ref); key != "" {
				currentIssues[key] = issue
			}
		}
	}
	decisions := make([]operations.Decision, 0, len(report.Decisions))
	for _, d := range report.Decisions {
		key := operationsDecisionKey(d.ProjectID, d.Issue, "")
		if key != "" && seen[key] {
			continue
		}
		issue, current := currentIssues[operationsProjectIssueKey(operationsProjectScope(d.ProjectID, snapshot.Project.ID), d.Issue)]
		if current && operationsPullRequestSupersedesQuestion(issue.PullRequest) {
			continue
		}
		// Match humanQuestionWaiting: only nonempty current evidence supersedes a question.
		if current && issue.PullRequest != nil && issue.PullRequest.HumanQuestionWorkFingerprint != "" && issue.PullRequest.HumanQuestionWorkFingerprint != d.WorkFingerprint {
			continue
		}
		d.Kind = "question"
		if current {
			d.Title = issue.Title
		}
		decisions = append(decisions, d)
		if key != "" {
			seen[key] = true
		}
	}
	report.Decisions = decisions
	blockedByIssue := map[string]telemetry.Blocked{}
	for _, row := range snapshot.Blocked {
		projectID := operationsProjectScope(row.ProjectID, snapshot.Project.ID)
		for _, ref := range []string{row.Identifier, row.ID} {
			if key := operationsProjectIssueKey(projectID, ref); key != "" {
				blockedByIssue[key] = row
			}
		}
	}
	for _, issue := range issues {
		key := operationsDecisionKey(issue.ProjectID, issue.Identifier, issue.ID)
		if issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil {
			entry := issue.PullRequest.MergeQueueEntry
			if entry.Depth > mergeDepths[issue.ProjectID] {
				mergeDepths[issue.ProjectID] = entry.Depth
			}
			if entry.MaxGroupSize > 0 && (report.MergeGroupSize == nil || entry.MaxGroupSize > *report.MergeGroupSize) {
				size, wait := entry.MaxGroupSize, entry.MinGroupWaitSeconds
				report.MergeGroupSize, report.MergeGroupWait = &size, &wait
			}
		}

		if !seen[key] {
			if d, ok := operationsHumanDependencyDecision(snapshot, issue, currentIssues); ok {
				report.Decisions = append(report.Decisions, d)
				seen[key] = true
			}
		}
		if !seen[key] {
			ref := strings.TrimSpace(issue.Identifier)
			if ref == "" {
				ref = strings.TrimSpace(issue.ID)
			}
			row, ok := blockedByIssue[operationsProjectIssueKey(operationsProjectScope(issue.ProjectID, snapshot.Project.ID), ref)]
			if ok && (row.NeedsHumanAttention || strings.EqualFold(strings.TrimSpace(row.RecoveryAction), "hold")) {
				report.Decisions = append(report.Decisions, operationsBlockedDecision(row))
				seen[key] = true
			}
		}
		if !seen[key] {
			if d, ok := operationsRequiredGateDecision(issue, s.operationsProjectHumanReviewRequired(issue, snapshot.Project.ID)); ok {
				report.Decisions = append(report.Decisions, d)
				seen[key] = true
			}
		}
		for i := range report.Actions {
			a := &report.Actions[i]
			if a.ProjectID == issue.ProjectID && a.Issue == issue.ID {
				a.Issue = issue.Identifier
				a.EvidenceURL = issue.URL
			}
		}
	}
	for _, queued := range snapshot.Queue {
		if strings.EqualFold(queued.State, "Merging") && mergeDepths[queued.ProjectID] == 0 {
			report.QueueDepth++
		}
	}
	for _, depth := range mergeDepths {
		report.QueueDepth += depth
	}
	for i := range report.Actions {
		a := &report.Actions[i]
		if a.EvidenceURL == "" {
			a.EvidenceURL = "/api/v1/projects/" + url.PathEscape(a.ProjectID) + "/issues/explanation?reference=" + url.QueryEscape(a.Issue)
		}
	}
	for i := range report.Decisions {
		d := &report.Decisions[i]
		if d.URL == "" {
			d.URL = "/api/v1/projects/" + url.PathEscape(d.ProjectID) + "/issues/explanation?reference=" + url.QueryEscape(d.Issue)
		}
	}
	sort.Slice(report.Decisions, func(i, j int) bool {
		a, b := report.Decisions[i], report.Decisions[j]
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.Issue < b.Issue
	})
	return report, nil
}

func operationsDecisionKey(projectID string, identifier string, issueID string) string {
	projectID = strings.ToLower(strings.TrimSpace(projectID))
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if operationsGlobalIssueIdentifier(identifier) {
		return "identifier\x00" + identifier
	}
	if identifier != "" {
		return "project\x00" + projectID + "\x00identifier\x00" + identifier
	}
	issueID = strings.ToLower(strings.TrimSpace(issueID))
	if issueID == "" {
		return ""
	}
	return "project\x00" + projectID + "\x00issue\x00" + issueID
}

func operationsProjectScope(projectID string, fallback string) string {
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		return projectID
	}
	return strings.TrimSpace(fallback)
}

func operationsProjectIssueKey(projectID string, ref string) string {
	projectID = strings.ToLower(strings.TrimSpace(projectID))
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return ""
	}
	return "project\x00" + projectID + "\x00ref\x00" + ref
}

func operationsGlobalIssueIdentifier(identifier string) bool {
	repository, number, found := strings.Cut(strings.TrimSpace(identifier), "#")
	if !found || strings.Count(repository, "/") != 1 || number == "" {
		return false
	}
	return strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) == -1
}

func operationsPullRequestSupersedesQuestion(pr *telemetry.PullRequest) bool {
	if pr == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(pr.State)) {
	case "closed", "merged":
		return true
	default:
		return false
	}
}

func operationsHumanDependencyDecision(snapshot telemetry.Snapshot, issue telemetry.Issue, issuesByKey map[string]telemetry.Issue) (operations.Decision, bool) {
	latest, found := operationsLatestSchedulerDecision(snapshot, issue)
	if !strings.EqualFold(strings.TrimSpace(issue.State), "Todo") || !found || !strings.EqualFold(strings.TrimSpace(latest.Lane), "Todo") || latest.Result != "skipped" || latest.Reason != "blocked_by_dependency" {
		return operations.Decision{}, false
	}
	for _, ref := range issue.BlockedBy {
		if !ref.HumanOwned || ref.HumanCompletionReady {
			continue
		}
		identifier := strings.TrimSpace(ref.Identifier)
		if identifier == "" {
			identifier = strings.TrimSpace(ref.ID)
		}
		prerequisite := issuesByKey[operationsProjectIssueKey(operationsProjectScope(issue.ProjectID, snapshot.Project.ID), identifier)]
		return operations.Decision{
			Kind:      "human_prerequisite",
			ProjectID: issue.ProjectID,
			Issue:     issue.Identifier,
			Title:     issue.Title,
			Question:  "Complete and close the human-owned prerequisite.",
			URL:       issue.URL,
			Prerequisite: &operations.Prerequisite{
				Issue:    identifier,
				Title:    prerequisite.Title,
				URL:      operationsIssueURL(prerequisite.URL, identifier, prerequisite.ProjectID, issue.ProjectID),
				Evidence: "Completion evidence is required.",
			},
		}, true
	}
	return operations.Decision{}, false
}

func operationsLatestSchedulerDecision(snapshot telemetry.Snapshot, issue telemetry.Issue) (telemetry.SchedulerDecision, bool) {
	var latest telemetry.SchedulerDecision
	found := false
	for _, decision := range snapshot.SchedulerDecisions {
		if !operationsSchedulerDecisionMatches(snapshot, issue, decision) {
			continue
		}
		if !found || decision.DecisionAt.After(latest.DecisionAt) {
			latest = decision
			found = true
		}
	}
	return latest, found
}

func operationsSchedulerDecisionMatches(snapshot telemetry.Snapshot, issue telemetry.Issue, decision telemetry.SchedulerDecision) bool {
	projectID := strings.TrimSpace(issue.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(snapshot.Project.ID)
	}
	decisionProjectID := strings.TrimSpace(decision.ProjectID)
	if decisionProjectID == "" {
		decisionProjectID = strings.TrimSpace(snapshot.Project.ID)
	}
	if !strings.EqualFold(projectID, decisionProjectID) {
		return false
	}
	if strings.TrimSpace(issue.ID) != "" && strings.TrimSpace(decision.IssueID) != "" {
		return strings.TrimSpace(issue.ID) == strings.TrimSpace(decision.IssueID)
	}
	return strings.EqualFold(strings.TrimSpace(issue.Identifier), strings.TrimSpace(decision.Identifier))
}

func operationsBlockedDecision(row telemetry.Blocked) operations.Decision {
	reason := strings.TrimSpace(row.Error)
	if reason == "" {
		reason = strings.ReplaceAll(strings.TrimSpace(row.RecoveryReason), "_", " ")
	}
	if reason == "" {
		reason = "Blocked park has no automatic exit."
	}
	if remedy := strings.TrimSpace(row.RecoveryRemedy); remedy != "" && !strings.Contains(reason, remedy) {
		reason += " — " + remedy
	}
	return operations.Decision{Kind: "blocked_park", ProjectID: row.ProjectID, Issue: row.Identifier, Title: row.Title, Question: reason, URL: row.URL}
}

func operationsRequiredGateDecision(issue telemetry.Issue, projectHumanReviewRequired bool) (operations.Decision, bool) {
	humanAction := ""
	if issue.RequiredGate != nil {
		humanAction = strings.TrimSpace(issue.RequiredGate.HumanAction)
	}
	pullRequestClosed := operationsPullRequestSupersedesQuestion(issue.PullRequest)
	isPullRequestReview := strings.EqualFold(strings.TrimSpace(issue.State), "Human Review") && issue.PullRequest != nil && !pullRequestClosed
	if humanAction == "" && projectHumanReviewRequired && isPullRequestReview {
		humanAction = "Review the pull request."
		if issue.PullRequest.Number > 0 {
			humanAction = "Review pull request #" + strconv.Itoa(issue.PullRequest.Number) + "."
		}
	}
	if humanAction == "" {
		return operations.Decision{}, false
	}
	decision := operations.Decision{Kind: "required_gate", ProjectID: issue.ProjectID, Issue: issue.Identifier, Title: issue.Title, Question: humanAction, URL: issue.URL}
	if !isPullRequestReview {
		return decision, true
	}
	decision.Kind = "pull_request_review"
	if pullRequestURL := strings.TrimSpace(issue.PullRequest.URL); pullRequestURL != "" {
		decision.URL = pullRequestURL
	}
	return decision, true
}

func (s *Server) operationsProjectHumanReviewRequired(issue telemetry.Issue, fallbackProjectID string) bool {
	if s.registry == nil {
		return false
	}
	trackedProject, ok := s.registry.Get(projectpkg.ID(operationsProjectScope(issue.ProjectID, fallbackProjectID)))
	if !ok || trackedProject == nil {
		return false
	}
	workflow := trackedProject.Workflow().Config
	if !workflow.Agent.AutoPromote.Enabled {
		return true
	}
	if operationsLabelsIntersect(issue.Labels, []string{workflow.Agent.AutoPromote.OptoutLabel}) {
		return true
	}
	if allowed := workflow.Agent.AutoPromote.AllowedIssueLabels; len(allowed) > 0 && !operationsLabelsIntersect(issue.Labels, allowed) {
		return true
	}
	return gate.NormalizeKind(workflow.Gate.Kind) == gate.KindHumanReview
}

func operationsLabelsIntersect(labels []string, candidates []string) bool {
	for _, label := range labels {
		for _, candidate := range candidates {
			if candidate = strings.TrimSpace(candidate); candidate != "" && strings.EqualFold(strings.TrimSpace(label), candidate) {
				return true
			}
		}
	}
	return false
}

func operationsIssueURL(current string, identifier string, projectIDs ...string) string {
	if current = strings.TrimSpace(current); current != "" {
		return current
	}
	repo, number, ok := strings.Cut(strings.TrimSpace(identifier), "#")
	if ok && strings.Count(repo, "/") == 1 && number != "" {
		return "https://github.com/" + repo + "/issues/" + number
	}
	for _, projectID := range projectIDs {
		if projectID = strings.TrimSpace(projectID); projectID != "" {
			return "/api/v1/projects/" + url.PathEscape(projectID) + "/issues/explanation?reference=" + url.QueryEscape(strings.TrimSpace(identifier))
		}
	}
	return ""
}

func (s *Server) apiOperations(c echo.Context) error {
	report, err := s.operationsReport(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Server) operationsPage(c echo.Context) error {
	report, err := s.operationsReport(c)
	if err != nil {
		return err
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		return render(c, templates.OperationsSnapshot(report))
	}
	data := s.analyticsDashboardData(c.Request().Context(), s.latestSnapshot(c.Request().Context()))
	data.ActiveNav = "operations"
	applyDashboardPreferences(c.Request(), &data)
	shell := templates.DashboardShellDataFromDashboard(data)
	shell.Title = instancePageTitle(report.Instance, "Operations")
	return render(c, templates.OperationsPage(shell, report))
}
