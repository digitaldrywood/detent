package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

const doctorAutoPromoteSampleLimit = 5
const (
	doctorReviewFlowAutopilot  = "autopilot"
	doctorReviewFlowReviewGate = "review-gate"
)

type doctorAutoPromoteCandidateDiagnostic struct {
	IssueID                      string     `json:"issue_id,omitempty"`
	IssueIdentifier              string     `json:"issue_identifier,omitempty"`
	IssueURL                     string     `json:"issue_url,omitempty"`
	PRNumber                     int        `json:"pr_number,omitempty"`
	PRURL                        string     `json:"pr_url,omitempty"`
	PRHeadSHA                    string     `json:"pr_head_sha,omitempty"`
	PRMergeableState             string     `json:"pr_mergeable_state,omitempty"`
	LatestCodexReviewState       string     `json:"latest_codex_review_state,omitempty"`
	LatestCodexReviewCommitSHA   string     `json:"latest_codex_review_commit_sha,omitempty"`
	LatestCodexReviewSubmittedAt *time.Time `json:"latest_codex_review_submitted_at,omitempty"`
	QuietRemainingSeconds        int64      `json:"quiet_remaining_seconds,omitempty"`
	WorkpadBlocker               string     `json:"workpad_blocker,omitempty"`
	WorkpadCommentURL            string     `json:"workpad_comment_url,omitempty"`
	WorkpadSignalSource          string     `json:"workpad_signal_source,omitempty"`
	WorkpadStatusInvalid         string     `json:"workpad_status_invalid,omitempty"`
	WorkpadStatusInvalidHash     string     `json:"workpad_status_invalid_hash,omitempty"`
	WorkpadProseFallbackDisabled bool       `json:"workpad_prose_fallback_disabled,omitempty"`
	Reason                       string     `json:"reason"`
}

type doctorStatusDriftIssueDiagnostic struct {
	IssueID         string   `json:"issue_id,omitempty"`
	IssueIdentifier string   `json:"issue_identifier,omitempty"`
	IssueURL        string   `json:"issue_url,omitempty"`
	State           string   `json:"state,omitempty"`
	Labels          []string `json:"labels,omitempty"`
}

type doctorAutoPromoteConnector interface {
	FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error)
}

type doctorAutoPromoteLimitedConnector interface {
	FetchIssuesByStatesLimit(context.Context, []string, int) ([]connector.Issue, error)
}

type doctorStatusOptionVerifier interface {
	VerifyStatusOptions(context.Context, []string) error
}

func checkDoctorAutoPromote(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps, now time.Time) doctorCheck {
	name := "Project " + id + " auto-promote"
	flowDetail := doctorReviewFlowConfigDetail(cfg)
	if !cfg.Agent.AutoPromote.Enabled {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: flowDetail + "; live candidate diagnostics disabled",
		}
	}
	passState := strings.TrimSpace(cfg.Agent.AutoPromote.PassState)
	if passState == "" {
		passState = "Merging"
	}
	reworkState := strings.TrimSpace(cfg.Agent.AutoPromote.ReworkState)
	if reworkState == "" {
		reworkState = "Rework"
	}
	if !doctorStateInList(passState, cfg.Tracker.ActiveStates) && !doctorStateInList(passState, cfg.Tracker.TerminalStates) {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "agent.auto_promote.enabled=true but pass_state " + passState + " is not configured in tracker.active_states or tracker.terminal_states",
			Hint:   "Add " + passState + " to tracker.active_states or tracker.terminal_states.",
		}
	}
	if !doctorStateInList(reworkState, cfg.Tracker.ActiveStates) && !doctorStateInList(reworkState, cfg.Tracker.TerminalStates) {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "agent.auto_promote.enabled=true but rework_state " + reworkState + " is not configured in tracker.active_states or tracker.terminal_states",
			Hint:   "Add " + reworkState + " to tracker.active_states or tracker.terminal_states.",
		}
	}
	if cfg.Agent.AutoPromote.ReworkLimit > 0 &&
		!doctorStateInList("Blocked", cfg.Tracker.ActiveStates) &&
		!doctorStateInList("Blocked", cfg.Tracker.ObservedStates) &&
		!doctorStateInList("Blocked", cfg.Tracker.TerminalStates) {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "agent.auto_promote.rework_limit is greater than 0 but Blocked is not configured in tracker.active_states, tracker.observed_states, or tracker.terminal_states",
			Hint:   "Add Blocked to a tracker state list so rework-limit handoff status writes can be provisioned and verified.",
		}
	}
	if !doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: flowDetail + "; live GitHub diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create auto-promote diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and ProjectV2 configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create auto-promote diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorAutoPromoteLive(ctx, name, cfg, projectConnector, now)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorAutoPromoteLive(
	ctx context.Context,
	name string,
	cfg workflowconfig.Config,
	projectConnector doctorAutoPromoteConnector,
	now time.Time,
) doctorCheck {
	sourceState := strings.TrimSpace(cfg.Agent.AutoPromote.SourceState)
	if sourceState == "" {
		sourceState = "Human Review"
	}
	passState := strings.TrimSpace(cfg.Agent.AutoPromote.PassState)
	if passState == "" {
		passState = "Merging"
	}
	reworkState := strings.TrimSpace(cfg.Agent.AutoPromote.ReworkState)
	if reworkState == "" {
		reworkState = "Rework"
	}
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		verifyStates := []string{sourceState, passState, reworkState}
		if cfg.Agent.AutoPromote.ReworkLimit > 0 {
			verifyStates = append(verifyStates, "Blocked")
		}
		if err := verifier.VerifyStatusOptions(ctx, verifyStates); err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("status option verification failed: %v", err),
				Hint:   "Ensure auto-promote source, pass, rework, and rework-limit target states resolve through tracker.state_map to existing GitHub Project Status options.",
			}
		}
	}

	scan, err := fetchDoctorAutoPromoteIssues(ctx, projectConnector, []string{sourceState})
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch %s candidates: %v", sourceState, err),
			Hint:   "Check GitHub Project access, Status field options, and repository pull request access.",
		}
	}
	issues := scan.Issues

	autoPromoteCfg := doctorAutoPromoteConfig(cfg)
	reasonCounts := map[string]int{}
	candidates := make([]doctorAutoPromoteCandidateDiagnostic, 0, len(issues))
	var quietRemaining time.Duration
	for _, issue := range issues {
		issue, err = doctorHydrateAutoPromoteWorkpad(ctx, projectConnector, issue)
		if err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("fetch Workpad comments for %s diagnostics: %v", sourceState, err),
				Hint:   "Check GitHub issue comments read access and repository rate limits.",
			}
		}
		summary := orchestrator.AutoPromoteSummaryFromIssue(issue)
		decision := orchestrator.EvaluateAutoPromote(issue, summary, autoPromoteCfg, now)
		candidate := doctorAutoPromoteCandidateDiagnosticFromIssue(issue, decision)
		candidates = append(candidates, candidate)
		reasonCounts[candidate.Reason]++
		if decision.QuietRemaining > quietRemaining {
			quietRemaining = decision.QuietRemaining
		}
		if decision.Reason == orchestrator.AutoPromoteReasonMissingPullRequest {
			if prNumber, ok := doctorLinkedPullRequestNumber(issue); ok {
				return doctorCheck{
					Name:                  name,
					Status:                doctorFail,
					Detail:                fmt.Sprintf("%s has linked PR #%d but auto-promote readiness reports missing_pull_request", doctorIssueLabel(issue), prNumber),
					Hint:                  "Verify GitHub PR attachment, branch prefix matching, and repository access for Human Review candidates.",
					AutoPromoteCandidates: []doctorAutoPromoteCandidateDiagnostic{candidate},
				}
			}
		}
	}

	detail := fmt.Sprintf(
		"%s; status options resolved; sampled %d %s candidate(s) of %d enumerated",
		doctorReviewFlowConfigDetail(cfg),
		len(issues),
		sourceState,
		doctorStateScanCount(scan.EnumeratedCounts, sourceState),
	)
	if cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceProjectV2 && (scan.ItemsFetched > 0 || scan.TotalItems > 0) {
		detail += fmt.Sprintf("; ProjectV2 items fetched=%d total=%d", scan.ItemsFetched, scan.TotalItems)
	}
	boardCount := doctorStateScanCount(scan.BoardCounts, sourceState)
	enumeratedCount := doctorStateScanCount(scan.EnumeratedCounts, sourceState)
	detail += fmt.Sprintf("; %s counts board=%d enumerated=%d", sourceState, boardCount, enumeratedCount)
	if len(reasonCounts) > 0 {
		detail += "; reasons: " + doctorAutoPromoteReasonCounts(reasonCounts)
	}
	if quietRemaining > 0 {
		detail += "; max_quiet_remaining=" + quietRemaining.Truncate(time.Second).String()
	}
	if len(candidates) > 0 {
		detail += "; candidates: " + doctorAutoPromoteCandidateSummaries(candidates)
	}
	if boardCount != enumeratedCount {
		return doctorCheck{
			Name:                  name,
			Status:                doctorFail,
			Detail:                detail + "; board fetch and candidate enumeration disagree",
			Hint:                  "Upgrade Detent or inspect ProjectV2 item pagination; auto-promote cannot safely act while review-state counts disagree.",
			AutoPromoteCandidates: candidates,
		}
	}
	check := doctorCheck{
		Name:                  name,
		Status:                doctorOK,
		Detail:                detail,
		AutoPromoteCandidates: candidates,
	}
	if invalid := doctorAutoPromoteInvalidWorkpadStatusCandidates(candidates); len(invalid) > 0 {
		check.Status = doctorWarn
		check.Detail += "; invalid workpad status candidate(s): " + doctorAutoPromoteCandidateSummaries(invalid)
		check.Hint = "Update the Workpad detent-status block to one of in_progress, blocked, or complete; if this repeats, align WORKFLOW.md handoff prose with the configured review flow."
	}
	merged, reviewed, err := doctorAutomatedReviewProducerSample(ctx, cfg, projectConnector)
	if err != nil {
		check.Status = doctorWarn
		check.Detail += "; automated-review producer health unavailable: " + err.Error()
		check.Hint = "Check terminal issue and merged pull request read access, then rerun detent doctor."
	} else if merged > 0 {
		check.Detail += fmt.Sprintf("; recent merged PR automated reviews=%d/%d", reviewed, merged)
		if reviewed == 0 {
			check.Status = doctorWarn
			check.Detail += "; automated-review producer appears inactive"
			check.Hint = "Restore the automated review producer or use gate.automated_review: optional so a missing review cannot strand merging."
		}
	}
	return check
}

func doctorAutomatedReviewProducerSample(
	ctx context.Context,
	cfg workflowconfig.Config,
	projectConnector doctorAutoPromoteConnector,
) (int, int, error) {
	mode := gate.AutomatedReviewMode(cfg.Gate)
	if mode == gate.AutomatedReviewOff {
		return 0, 0, nil
	}
	states := append([]string(nil), cfg.Tracker.TerminalStates...)
	if len(states) == 0 {
		states = []string{"Done"}
	}
	scan, err := fetchDoctorAutoPromoteIssues(ctx, projectConnector, states)
	if err != nil {
		return 0, 0, err
	}
	merged := 0
	reviewed := 0
	for _, issue := range scan.Issues {
		if hydrator, ok := projectConnector.(connector.PullRequestHydrator); ok && issue.PullRequest != nil {
			issue, err = hydrator.HydratePullRequest(ctx, issue)
			if err != nil {
				return 0, 0, fmt.Errorf("hydrate recent merged pull request for %s: %w", doctorIssueLabel(issue), err)
			}
		}
		if issue.PullRequest == nil || !strings.EqualFold(strings.TrimSpace(issue.PullRequest.State), "merged") {
			continue
		}
		merged++
		if doctorLatestCodexReviewState(issue.PullRequest) != "" || doctorLatestCodexReviewSubmittedAt(issue.PullRequest) != nil {
			reviewed++
		}
	}
	return merged, reviewed, nil
}

func doctorReviewFlowChoice(cfg workflowconfig.Config) string {
	if cfg.Agent.AutoPromote.Enabled &&
		cfg.Agent.AutoPromote.QuietSeconds == 0 &&
		doctorReviewFlowGateWaitState(cfg) == workflowconfig.AutoPromoteGateWaitStateSource {
		return doctorReviewFlowAutopilot
	}
	return doctorReviewFlowReviewGate
}

func doctorReviewFlowConfigDetail(cfg workflowconfig.Config) string {
	return fmt.Sprintf(
		"review-flow=%s (auto_promote.enabled=%t, quiet_seconds=%d, gate_wait_state=%s, automated_review=%s, gate_wait_timeout_action=%s)",
		doctorReviewFlowChoice(cfg),
		cfg.Agent.AutoPromote.Enabled,
		cfg.Agent.AutoPromote.QuietSeconds,
		doctorReviewFlowGateWaitState(cfg),
		gate.AutomatedReviewMode(cfg.Gate),
		workflowconfig.EffectiveAutoPromoteGateWaitTimeoutAction(cfg.Agent.AutoPromote.GateWaitTimeoutAction, cfg.Gate),
	)
}

func doctorReviewFlowGateWaitState(cfg workflowconfig.Config) string {
	state := strings.ToLower(strings.TrimSpace(cfg.Agent.AutoPromote.GateWaitState))
	if state == "" {
		return workflowconfig.AutoPromoteGateWaitStateSource
	}
	return state
}

func doctorAutoPromoteInvalidWorkpadStatusCandidates(candidates []doctorAutoPromoteCandidateDiagnostic) []doctorAutoPromoteCandidateDiagnostic {
	out := []doctorAutoPromoteCandidateDiagnostic{}
	for _, candidate := range candidates {
		if candidate.Reason == string(orchestrator.AutoPromoteReasonWorkpadStatusInvalid) {
			out = append(out, candidate)
		}
	}
	return out
}

func doctorHydrateAutoPromoteWorkpad(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	issue connector.Issue,
) (connector.Issue, error) {
	if len(issue.Comments) > 0 {
		return issue, nil
	}
	reader, ok := projectConnector.(connector.IssueCommentReader)
	if !ok {
		return issue, nil
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		return issue, err
	}
	issue.Comments = comments
	return issue, nil
}

func fetchDoctorAutoPromoteIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	states []string,
) (connector.IssueStateScan, error) {
	if scanner, ok := projectConnector.(connector.IssueStateScanner); ok {
		return scanner.FetchIssuesByStatesScan(ctx, states, doctorAutoPromoteSampleLimit)
	}
	if limited, ok := projectConnector.(doctorAutoPromoteLimitedConnector); ok {
		issues, err := limited.FetchIssuesByStatesLimit(ctx, states, doctorAutoPromoteSampleLimit)
		return doctorIssueStateScan(issues), err
	}
	issues, err := projectConnector.FetchIssuesByStates(ctx, states)
	if err != nil {
		return connector.IssueStateScan{}, err
	}
	scan := doctorIssueStateScan(issues)
	if len(scan.Issues) > doctorAutoPromoteSampleLimit {
		scan.Issues = scan.Issues[:doctorAutoPromoteSampleLimit]
	}
	return scan, nil
}

func doctorIssueStateScan(issues []connector.Issue) connector.IssueStateScan {
	scan := connector.IssueStateScan{
		Issues:           issues,
		BoardCounts:      map[string]int{},
		EnumeratedCounts: map[string]int{},
		ItemsFetched:     len(issues),
		TotalItems:       len(issues),
	}
	for _, issue := range issues {
		scan.BoardCounts[issue.State]++
		scan.EnumeratedCounts[issue.State]++
	}
	return scan
}

func doctorStateScanCount(counts map[string]int, state string) int {
	state = strings.TrimSpace(state)
	for candidate, count := range counts {
		if strings.EqualFold(strings.TrimSpace(candidate), state) {
			return count
		}
	}
	return 0
}

func checkDoctorLabelStatusDrift(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " label status drift"
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub || cfg.Tracker.GitHubStatusSource != workflowconfig.GitHubStatusSourceLabel {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "label status drift diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create label status drift diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and repository configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create label status drift diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorLabelStatusDriftLive(ctx, name, projectConnector)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorLabelStatusDriftLive(
	ctx context.Context,
	name string,
	projectConnector doctorAutoPromoteConnector,
) doctorCheck {
	reader, ok := projectConnector.(connector.StatusDriftReader)
	if !ok {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "GitHub connector does not support label status drift diagnostics",
			Hint:   "Upgrade the configured connector or run a current Detent build.",
		}
	}
	drift, err := reader.FetchStatusDrift(ctx)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch label status drift: %v", err),
			Hint:   "Check GitHub repository issue access and rate limits.",
		}
	}

	untracked := doctorStatusDriftDiagnostics(drift.UntrackedOpen)
	openTerminal := doctorStatusDriftDiagnostics(drift.OpenTerminal)
	closedActive := doctorStatusDriftDiagnostics(drift.ClosedActive)
	if len(untracked) == 0 && len(openTerminal) == 0 && len(closedActive) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "sampled repository issues; 0 untracked open issues, 0 open terminal issues, and 0 closed active issues",
		}
	}

	detail := fmt.Sprintf(
		"label status drift: %d open issue(s) without configured status label",
		len(untracked),
	)
	if len(untracked) > 0 {
		detail += ": " + doctorStatusDriftIssueSummaries(untracked)
	}
	detail += fmt.Sprintf("; %d open issue(s) with terminal status label", len(openTerminal))
	if len(openTerminal) > 0 {
		detail += ": " + doctorStatusDriftIssueSummaries(openTerminal)
	}
	detail += fmt.Sprintf("; %d closed issue(s) with active status label", len(closedActive))
	if len(closedActive) > 0 {
		detail += ": " + doctorStatusDriftIssueSummaries(closedActive)
	}
	return doctorCheck{
		Name:               name,
		Status:             doctorWarn,
		Detail:             detail,
		Hint:               "Apply exactly one configured status label to untracked open issues; close landed terminal-labeled issues, report terminal issues without landed work, and move closed active-labeled issues to a terminal lane.",
		UntrackedIssues:    untracked,
		OpenTerminalIssues: openTerminal,
		ClosedActiveIssues: closedActive,
	}
}

func doctorStatusDriftDiagnostics(issues []connector.Issue) []doctorStatusDriftIssueDiagnostic {
	out := make([]doctorStatusDriftIssueDiagnostic, 0, len(issues))
	for _, issue := range issues {
		out = append(out, doctorStatusDriftIssueDiagnostic{
			IssueID:         strings.TrimSpace(issue.ID),
			IssueIdentifier: strings.TrimSpace(issue.Identifier),
			IssueURL:        strings.TrimSpace(issue.URL),
			State:           strings.TrimSpace(issue.State),
			Labels:          append([]string(nil), issue.Labels...),
		})
	}
	return out
}

func doctorStatusDriftIssueSummaries(issues []doctorStatusDriftIssueDiagnostic) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, doctorStatusDriftIssueSummary(issue))
	}
	return strings.Join(parts, "; ")
}

func doctorStatusDriftIssueSummary(issue doctorStatusDriftIssueDiagnostic) string {
	label := strings.TrimSpace(issue.IssueIdentifier)
	if label == "" {
		label = strings.TrimSpace(issue.IssueID)
	}
	if label == "" {
		label = "sampled issue"
	}
	if url := strings.TrimSpace(issue.IssueURL); url != "" {
		return label + " " + url
	}
	return label
}

func doctorAutoPromoteConfig(cfg workflowconfig.Config) orchestrator.AutoPromoteConfig {
	return orchestrator.AutoPromoteConfig{
		Enabled:               cfg.Agent.AutoPromote.Enabled,
		QuietDuration:         time.Duration(cfg.Agent.AutoPromote.QuietSeconds) * time.Second,
		OptoutLabel:           cfg.Agent.AutoPromote.OptoutLabel,
		AllowedIssueLabels:    append([]string(nil), cfg.Agent.AutoPromote.AllowedIssueLabels...),
		GateWaitTimeoutAction: cfg.Agent.AutoPromote.GateWaitTimeoutAction,
		SourceState:           cfg.Agent.AutoPromote.SourceState,
		PassState:             cfg.Agent.AutoPromote.PassState,
		ReworkState:           cfg.Agent.AutoPromote.ReworkState,
		ReworkLimit:           cfg.Agent.AutoPromote.ReworkLimit,
		TerminalStates:        append([]string(nil), cfg.Tracker.TerminalStates...),
		WorkpadStructuredOnly: cfg.Workpad.StructuredOnly,
		Gate:                  cfg.Gate,
	}
}

func doctorAutoPromoteCandidateDiagnosticFromIssue(
	issue connector.Issue,
	decision orchestrator.AutoPromoteDecision,
) doctorAutoPromoteCandidateDiagnostic {
	diagnostic := doctorAutoPromoteCandidateDiagnostic{
		IssueID:         strings.TrimSpace(issue.ID),
		IssueIdentifier: strings.TrimSpace(issue.Identifier),
		IssueURL:        strings.TrimSpace(issue.URL),
		Reason:          doctorAutoPromoteDiagnosticReason(issue, decision),
	}
	if decision.QuietRemaining > 0 {
		diagnostic.QuietRemainingSeconds = int64(decision.QuietRemaining.Truncate(time.Second) / time.Second)
	}
	diagnostic.WorkpadBlocker = strings.TrimSpace(decision.WorkpadBlocker)
	diagnostic.WorkpadCommentURL = strings.TrimSpace(decision.WorkpadCommentURL)
	diagnostic.WorkpadSignalSource = strings.TrimSpace(decision.WorkpadSignalSource)
	diagnostic.WorkpadStatusInvalid = strings.TrimSpace(decision.WorkpadStatusInvalid)
	diagnostic.WorkpadStatusInvalidHash = strings.TrimSpace(decision.WorkpadStatusInvalidHash)
	diagnostic.WorkpadProseFallbackDisabled = decision.WorkpadProseFallbackDisabled
	if prNumber, ok := doctorLinkedPullRequestNumber(issue); ok {
		diagnostic.PRNumber = prNumber
	}
	if issue.PullRequest == nil {
		return diagnostic
	}

	pullRequest := issue.PullRequest
	if pullRequest.Number > 0 {
		diagnostic.PRNumber = pullRequest.Number
	}
	diagnostic.PRURL = strings.TrimSpace(pullRequest.URL)
	diagnostic.PRHeadSHA = strings.TrimSpace(pullRequest.HeadSHA)
	diagnostic.PRMergeableState = strings.TrimSpace(pullRequest.MergeableState)
	diagnostic.LatestCodexReviewState = doctorLatestCodexReviewState(pullRequest)
	diagnostic.LatestCodexReviewCommitSHA = strings.TrimSpace(pullRequest.LatestCodexReviewCommitSHA)
	diagnostic.LatestCodexReviewSubmittedAt = doctorLatestCodexReviewSubmittedAt(pullRequest)
	return diagnostic
}

func doctorAutoPromoteDiagnosticReason(issue connector.Issue, decision orchestrator.AutoPromoteDecision) string {
	if decision.Reason == orchestrator.AutoPromoteReasonCodexReviewMissing && doctorPullRequestHasStaleCodexReview(issue.PullRequest) {
		return "stale_automated_review"
	}
	if decision.Reason == orchestrator.AutoPromoteReasonCodexReviewNotQuiet {
		return "quiet_period_remaining"
	}
	return string(decision.Reason)
}

func doctorPullRequestHasStaleCodexReview(pullRequest *connector.PullRequest) bool {
	if pullRequest == nil {
		return false
	}
	headSHA := strings.TrimSpace(pullRequest.HeadSHA)
	reviewCommitSHA := strings.TrimSpace(pullRequest.LatestCodexReviewCommitSHA)
	if headSHA == "" || reviewCommitSHA == "" {
		return false
	}
	if doctorLatestCodexReviewState(pullRequest) == "" && pullRequest.LatestCodexReviewSubmittedAt == nil {
		return false
	}
	return !strings.EqualFold(headSHA, reviewCommitSHA)
}

func doctorLatestCodexReviewState(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	if state := strings.TrimSpace(pullRequest.LatestCodexReviewState); state != "" {
		return state
	}
	return strings.TrimSpace(pullRequest.CodexReviewState)
}

func doctorLatestCodexReviewSubmittedAt(pullRequest *connector.PullRequest) *time.Time {
	if pullRequest == nil {
		return nil
	}
	if pullRequest.LatestCodexReviewSubmittedAt != nil {
		return pullRequest.LatestCodexReviewSubmittedAt
	}
	return pullRequest.CodexReviewSubmittedAt
}

func doctorAutoPromoteCandidateSummaries(candidates []doctorAutoPromoteCandidateDiagnostic) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parts = append(parts, doctorAutoPromoteCandidateSummary(candidate))
	}
	return strings.Join(parts, "; ")
}

func doctorAutoPromoteCandidateSummary(candidate doctorAutoPromoteCandidateDiagnostic) string {
	parts := []string{doctorAutoPromoteCandidateIssueLabel(candidate)}
	if candidate.PRNumber > 0 {
		parts = append(parts, fmt.Sprintf("PR #%d", candidate.PRNumber))
	}
	if candidate.PRURL != "" {
		parts = append(parts, candidate.PRURL)
	}
	if candidate.PRHeadSHA != "" {
		parts = append(parts, "head="+candidate.PRHeadSHA)
	}
	if candidate.PRMergeableState != "" {
		parts = append(parts, "mergeable="+candidate.PRMergeableState)
	}
	if candidate.LatestCodexReviewState != "" || candidate.LatestCodexReviewCommitSHA != "" {
		review := strings.TrimSpace(candidate.LatestCodexReviewState)
		if candidate.LatestCodexReviewCommitSHA != "" {
			if review == "" {
				review = "review"
			}
			review += "@" + candidate.LatestCodexReviewCommitSHA
		}
		parts = append(parts, "review="+review)
	}
	if candidate.LatestCodexReviewSubmittedAt != nil {
		parts = append(parts, "submitted="+candidate.LatestCodexReviewSubmittedAt.UTC().Format(time.RFC3339))
	}
	if candidate.QuietRemainingSeconds > 0 {
		parts = append(parts, "quiet_remaining="+(time.Duration(candidate.QuietRemainingSeconds)*time.Second).String())
	}
	if candidate.WorkpadBlocker != "" {
		parts = append(parts, "workpad_blocker="+candidate.WorkpadBlocker)
	}
	if candidate.WorkpadSignalSource != "" {
		parts = append(parts, "workpad_signal_source="+candidate.WorkpadSignalSource)
	}
	if candidate.WorkpadCommentURL != "" {
		parts = append(parts, "workpad_comment_url="+candidate.WorkpadCommentURL)
	}
	if candidate.WorkpadStatusInvalid != "" {
		parts = append(parts, "workpad_status_invalid="+candidate.WorkpadStatusInvalid)
	}
	if candidate.WorkpadProseFallbackDisabled {
		parts = append(parts, "workpad_prose_fallback_disabled=true")
	}
	parts = append(parts, "reason="+candidate.Reason)
	return strings.Join(parts, " ")
}

func doctorAutoPromoteCandidateIssueLabel(candidate doctorAutoPromoteCandidateDiagnostic) string {
	switch {
	case candidate.IssueID != "" && candidate.IssueIdentifier != "":
		return candidate.IssueID + " (" + candidate.IssueIdentifier + ")"
	case candidate.IssueID != "":
		return candidate.IssueID
	case candidate.IssueIdentifier != "":
		return candidate.IssueIdentifier
	default:
		return "sampled issue"
	}
}

func doctorAutoPromoteReasonCounts(counts map[string]int) string {
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		if strings.TrimSpace(reason) != "" {
			reasons = append(reasons, reason)
		}
	}
	sort.Strings(reasons)

	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, counts[reason]))
	}
	return strings.Join(parts, ", ")
}

func closeDoctorAutoPromoteConnector(projectConnector doctorAutoPromoteConnector) error {
	closer, ok := projectConnector.(connector.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

func defaultDoctorAutoPromoteConnector(cfg workflowconfig.Config) (doctorAutoPromoteConnector, error) {
	return factory.NewFromConfig(factory.Config{
		Kind:   cfg.Tracker.Kind,
		Memory: memory.Config{Issues: cfg.Tracker.Issues},
		LocalSQLite: local.Config{
			Path:           cfg.Tracker.LocalSQLite.Path,
			ProjectID:      cfg.Tracker.LocalSQLite.ProjectID,
			Issues:         cfg.Tracker.Issues,
			ActiveStates:   cfg.Tracker.ActiveStates,
			ObservedStates: cfg.Tracker.ObservedStates,
			TerminalStates: cfg.Tracker.TerminalStates,
		},
		Endpoint:                    cfg.Tracker.Endpoint,
		APIKey:                      cfg.Tracker.APIKey,
		HTTPMaxIdleConns:            cfg.Tracker.HTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost:     cfg.Tracker.HTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeoutMS:       cfg.Tracker.HTTPIdleConnTimeoutMS,
		GitHubRESTMinReserve:        cfg.Tracker.GitHubRESTMinReserve,
		GitHubRESTFanoutMaxRequests: cfg.Tracker.GitHubRESTFanoutMaxRequests,
		GitHubUnstartedSeconds:      cfg.Tracker.GitHubUnstartedSeconds,
		GitHubRESTDebugLogging:      cfg.Tracker.GitHubRESTDebugLogging,
		GitHubAppID:                 cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:         cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath:     cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID:     cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:          cfg.Tracker.GitHubStatusSource,
		DependencySource:            cfg.Dependencies.Source,
		ProjectSlug:                 cfg.Tracker.ProjectSlug,
		Repository:                  cfg.Tracker.Repository,
		StatusField:                 cfg.Tracker.StatusField,
		StatusLabelPrefix:           cfg.Tracker.StatusLabelPrefix,
		ActiveStates:                cfg.Tracker.ActiveStates,
		ObservedStates:              cfg.Tracker.ObservedStates,
		TerminalStates:              cfg.Tracker.TerminalStates,
		StateMap:                    doctorTrackerStateMap(cfg.Tracker.StateMap),
		RequiredStatusChecks:        cfg.Gate.RequiredStatusChecks,
		Logger:                      doctorConnectorLogger(),
	})
}
