package orchestrator

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type AutoPromoteConfig struct {
	Enabled               bool
	QuietDuration         time.Duration
	OptoutLabel           string
	AllowedIssueLabels    []string
	GateWaitState         string
	GateWaitTimeout       time.Duration
	GateWaitTimeoutAction string
	SourceState           string
	PassState             string
	ReworkState           string
	ReworkLimit           int
	TerminalStates        []string
	NoProgressLimit       int
	WorkpadStructuredOnly bool
	Gate                  gate.Config
}

type AutoPromoteSummary struct {
	PullRequestPresent                    bool
	PullRequestURL                        string
	PullRequestHydrationUnavailableReason string
	PullRequestHydrationDegradedReason    string
	MergeableState                        string
	CIStatus                              string
	ReviewState                           string
	FailedChecks                          []string
	P1Findings                            []AutoPromoteFinding
	Validator                             gate.ValidatorResult
	ArtifactStatus                        string
	LastActivityAt                        *time.Time
	CompletedFinalState                   string
	AutomatedReviewWaitExpired            bool
}

type AutoPromoteFinding struct {
	Body string
	URL  string
	Path string
	Line int
}

type AutoPromoteAction string

const (
	AutoPromoteActionPromote     AutoPromoteAction = "promote"
	AutoPromoteActionRework      AutoPromoteAction = "rework"
	AutoPromoteActionAwaitReview AutoPromoteAction = "await_review"
	AutoPromoteActionSkip        AutoPromoteAction = "skip"
)

type AutoPromoteReason string

const (
	AutoPromoteReasonReady                           AutoPromoteReason = "ready"
	AutoPromoteReasonDisabled                        AutoPromoteReason = "disabled"
	AutoPromoteReasonOptoutLabel                     AutoPromoteReason = "optout_label"
	AutoPromoteReasonLabelNotAllowed                 AutoPromoteReason = "label_not_allowed"
	AutoPromoteReasonMissingPullRequest              AutoPromoteReason = "missing_pull_request"
	AutoPromoteReasonPullRequestMerged               AutoPromoteReason = "pull_request_merged"
	AutoPromoteReasonDraftPullRequest                AutoPromoteReason = "draft_pull_request"
	AutoPromoteReasonMergeRevocationLimit            AutoPromoteReason = "merge_revocation_limit"
	AutoPromoteReasonPullRequestHydrationUnavailable AutoPromoteReason = "pull_request_hydration_unavailable"
	AutoPromoteReasonMergeConflicts                  AutoPromoteReason = "merge_conflicts"
	AutoPromoteReasonCINotGreen                      AutoPromoteReason = "ci_not_green"
	AutoPromoteReasonCodexReviewMissing              AutoPromoteReason = "automated_review_missing"
	AutoPromoteReasonP1Findings                      AutoPromoteReason = "p1_findings"
	AutoPromoteReasonCodexReviewNotQuiet             AutoPromoteReason = "codex_review_not_quiet"
	AutoPromoteReasonHumanApprovalMissing            AutoPromoteReason = "human_approval_missing"
	AutoPromoteReasonValidatorMissing                AutoPromoteReason = "validator_missing"
	AutoPromoteReasonValidatorError                  AutoPromoteReason = "validator_error"
	AutoPromoteReasonValidatorWait                   AutoPromoteReason = "validator_wait"
	AutoPromoteReasonValidatorRework                 AutoPromoteReason = "validator_rework"
	AutoPromoteReasonValidatorScoreBelowThreshold    AutoPromoteReason = "validator_score_below_threshold"
	AutoPromoteReasonValidatorBlockedSeverity        AutoPromoteReason = "validator_blocked_severity"
	AutoPromoteReasonArtifactStatusMissing           AutoPromoteReason = "artifact_status_missing"
	AutoPromoteReasonArtifactStatusWait              AutoPromoteReason = "artifact_status_wait"
	AutoPromoteReasonArtifactStatusRework            AutoPromoteReason = "artifact_status_rework"
	AutoPromoteReasonWorkpadBlocker                  AutoPromoteReason = "workpad_blocker"
	AutoPromoteReasonWorkpadStatusInvalid            AutoPromoteReason = "workpad_status_invalid"
	AutoPromoteReasonWorkpadHydrationUnavailable     AutoPromoteReason = "workpad_hydration_unavailable"
)

type AutoPromoteDecision struct {
	Action                       AutoPromoteAction
	Reason                       AutoPromoteReason
	CIStatus                     string
	QuietRemaining               time.Duration
	Findings                     []AutoPromoteFinding
	WorkpadBlocker               string
	ResolvedWorkpadBlockers      []string
	WorkpadCommentURL            string
	WorkpadSignalSource          string
	WorkpadStatusInvalid         string
	WorkpadStatusInvalidHash     string
	WorkpadStatusInvalidContent  string
	WorkpadBlockerVerifications  []AutoPromoteWorkpadBlockerVerification
	WorkpadProseFallbackDisabled bool
}

type AutoPromoteWorkpadBlockerVerification struct {
	Identifier string
	State      string
	Status     string
	Reason     string
}

func EvaluateAutoPromote(
	issue connector.Issue,
	summary AutoPromoteSummary,
	cfg AutoPromoteConfig,
	now time.Time,
) AutoPromoteDecision {
	cfg = normalizeAutoPromoteConfig(cfg)

	if !cfg.Enabled {
		return autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonDisabled)
	}
	if autoPromoteOptoutLabel(issue, cfg) {
		return autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonOptoutLabel)
	}
	if !autoPromoteAllowedIssueLabel(issue, cfg) {
		return autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonLabelNotAllowed)
	}
	workpad := autoPromoteWorkpadBlocker(issue, cfg)
	if workpad.Invalid != nil {
		decision := autoPromoteDecision(autoPromoteInvalidWorkpadAction(summary.CompletedFinalState), AutoPromoteReasonWorkpadStatusInvalid)
		autoPromoteApplyWorkpadDecisionFields(&decision, workpad)
		return decision
	}
	if workpad.Reason != "" {
		decision := autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonWorkpadBlocker)
		decision.WorkpadBlocker = workpad.Reason
		autoPromoteApplyWorkpadDecisionFields(&decision, workpad)
		return decision
	}
	if gateRequiresPullRequest(cfg.Gate) {
		if issue.PullRequest != nil &&
			normalizePullRequestState(issue.PullRequest.State) == "open" &&
			issue.PullRequest.Draft {
			return autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonDraftPullRequest)
		}
		if autoPromoteMergeConflicts(summary.MergeableState) {
			return autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonMergeConflicts)
		}
		if strings.TrimSpace(summary.PullRequestHydrationUnavailableReason) != "" ||
			strings.TrimSpace(summary.PullRequestHydrationDegradedReason) != "" {
			return autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonPullRequestHydrationUnavailable)
		}
	}
	if status, ok := artifactStatusFieldFromIssue(issue, cfg.Gate.Artifact.StatusField); ok {
		summary.ArtifactStatus = status
	} else if !strings.EqualFold(cfg.Gate.Artifact.StatusField, gate.DefaultArtifactStatusField) {
		summary.ArtifactStatus = ""
	} else if strings.TrimSpace(summary.ArtifactStatus) == "" {
		summary.ArtifactStatus = artifactStatusFromIssue(issue, cfg.Gate.Artifact.StatusField)
	}
	gateDecision := gate.Evaluate(cfg.Gate, issue.Labels, gateSummary(summary), now, gate.EvaluationOptions{
		QuietDuration:              cfg.QuietDuration,
		AutomatedReviewWaitExpired: summary.AutomatedReviewWaitExpired,
	})
	decision := autoPromoteDecision(autoPromoteActionFromGate(gateDecision.Action), autoPromoteReasonFromGate(gateDecision.Reason))
	decision.CIStatus = gateDecision.CIStatus
	decision.QuietRemaining = gateDecision.QuietRemaining
	decision.Findings = autoPromoteFindingsFromGate(gateDecision.Findings)
	autoPromoteApplyWorkpadDecisionFields(&decision, workpad)
	return decision
}

func autoPromoteHumanReviewRequired(issue connector.Issue, cfg AutoPromoteConfig, gateCfg gate.Config) bool {
	cfg = normalizeAutoPromoteConfig(cfg)
	if !cfg.Enabled {
		return true
	}
	if autoPromoteOptoutLabel(issue, cfg) {
		return true
	}
	if !autoPromoteAllowedIssueLabel(issue, cfg) {
		return true
	}
	return gate.Effective(gateCfg).Kind == gate.KindHumanReview
}

func autoPromoteDecision(action AutoPromoteAction, reason AutoPromoteReason) AutoPromoteDecision {
	return AutoPromoteDecision{
		Action: action,
		Reason: reason,
	}
}

func cloneAutoPromoteDecisions(decisions map[string]AutoPromoteDecision) map[string]AutoPromoteDecision {
	if decisions == nil {
		return nil
	}
	cloned := make(map[string]AutoPromoteDecision, len(decisions))
	for issueID, decision := range decisions {
		cloned[issueID] = cloneAutoPromoteDecision(decision)
	}
	return cloned
}

func cloneAutoPromoteDecision(decision AutoPromoteDecision) AutoPromoteDecision {
	decision.Findings = append([]AutoPromoteFinding(nil), decision.Findings...)
	decision.ResolvedWorkpadBlockers = append([]string(nil), decision.ResolvedWorkpadBlockers...)
	decision.WorkpadBlockerVerifications = append([]AutoPromoteWorkpadBlockerVerification(nil), decision.WorkpadBlockerVerifications...)
	return decision
}

func autoPromoteInvalidWorkpadAction(completedFinalState string) AutoPromoteAction {
	if normalizeState(completedFinalState) == normalizeState(FinalStateCompleted) {
		return AutoPromoteActionRework
	}
	return AutoPromoteActionAwaitReview
}

func normalizeAutoPromoteConfig(cfg AutoPromoteConfig) AutoPromoteConfig {
	if cfg.QuietDuration < 0 {
		cfg.QuietDuration = 0
	}
	cfg.OptoutLabel = normalizeLabel(cfg.OptoutLabel)
	cfg.AllowedIssueLabels = normalizeLabels(cfg.AllowedIssueLabels)
	cfg.GateWaitState = normalizeAutoPromoteGateWaitState(cfg.GateWaitState)
	if cfg.GateWaitTimeout <= 0 {
		cfg.GateWaitTimeout = defaultAutoPromoteGateWaitTimeout
	}
	cfg.GateWaitTimeoutAction = normalizeAutoPromoteGateWaitTimeoutAction(cfg.GateWaitTimeoutAction, cfg.Gate)
	cfg.SourceState = strings.TrimSpace(cfg.SourceState)
	if cfg.SourceState == "" {
		cfg.SourceState = autoPromoteSourceState
	}
	cfg.PassState = strings.TrimSpace(cfg.PassState)
	if cfg.PassState == "" {
		cfg.PassState = autoPromoteMergingState
	}
	cfg.ReworkState = strings.TrimSpace(cfg.ReworkState)
	if cfg.ReworkState == "" {
		cfg.ReworkState = autoPromoteReworkState
	}
	if cfg.ReworkLimit < 0 {
		cfg.ReworkLimit = 0
	}
	cfg.TerminalStates = normalizedStates(cfg.TerminalStates)
	if cfg.NoProgressLimit < 0 {
		cfg.NoProgressLimit = 0
	}
	cfg.Gate = gate.Effective(cfg.Gate)
	return cfg
}

func normalizeAutoPromoteGateWaitTimeoutAction(action string, gateCfg gate.Config) string {
	switch normalizeState(action) {
	case autoPromoteGateWaitTimeoutMerge:
		return autoPromoteGateWaitTimeoutMerge
	case autoPromoteGateWaitTimeoutHumanReview, "human-review", "humanreview":
		return autoPromoteGateWaitTimeoutHumanReview
	}
	if gate.AutomatedReviewMode(gateCfg) == gate.AutomatedReviewRequired {
		return autoPromoteGateWaitTimeoutHumanReview
	}
	return autoPromoteGateWaitTimeoutMerge
}

func normalizeAutoPromoteGateWaitState(state string) string {
	switch normalizeState(state) {
	case "", autoPromoteGateWaitSource:
		return autoPromoteGateWaitSource
	case autoPromoteGateWaitReview:
		return autoPromoteGateWaitReview
	default:
		return normalizeState(state)
	}
}

func autoPromoteOptoutLabel(issue connector.Issue, cfg AutoPromoteConfig) bool {
	if cfg.OptoutLabel == "" {
		return false
	}

	for _, label := range issue.Labels {
		if normalizeLabel(label) == cfg.OptoutLabel {
			return true
		}
	}
	return false
}

func autoPromoteAllowedIssueLabel(issue connector.Issue, cfg AutoPromoteConfig) bool {
	if len(cfg.AllowedIssueLabels) == 0 {
		return true
	}

	allowed := make(map[string]struct{}, len(cfg.AllowedIssueLabels))
	for _, label := range cfg.AllowedIssueLabels {
		allowed[label] = struct{}{}
	}
	for _, label := range issue.Labels {
		if _, ok := allowed[normalizeLabel(label)]; ok {
			return true
		}
	}
	return false
}

func normalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

func normalizeLabels(labels []string) []string {
	normalized := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = normalizeLabel(label)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		normalized = append(normalized, label)
	}
	return normalized
}

func autoPromoteMergeConflicts(mergeableState string) bool {
	switch strings.ToLower(strings.TrimSpace(mergeableState)) {
	case "dirty", "conflicting":
		return true
	default:
		return false
	}
}

func gateSummary(summary AutoPromoteSummary) gate.Summary {
	return gate.Summary{
		PullRequestPresent: summary.PullRequestPresent,
		PullRequestURL:     summary.PullRequestURL,
		CIStatus:           summary.CIStatus,
		ReviewState:        summary.ReviewState,
		P1Findings:         gateFindings(summary.P1Findings),
		Validator:          summary.Validator,
		ArtifactStatus:     summary.ArtifactStatus,
		LastActivityAt:     summary.LastActivityAt,
	}
}

func gateRequiresPullRequest(cfg gate.Config) bool {
	return gate.Effective(cfg).Kind != gate.KindArtifact
}

func artifactStatusFromIssue(issue connector.Issue, statusField string) string {
	if status, ok := artifactStatusFieldFromIssue(issue, statusField); ok {
		return status
	}
	if statusField = strings.TrimSpace(statusField); statusField != "" &&
		!strings.EqualFold(statusField, gate.DefaultArtifactStatusField) {
		return ""
	}
	if issue.Deliverable != nil && strings.TrimSpace(issue.Deliverable.ValidationStatus) != "" {
		return strings.TrimSpace(issue.Deliverable.ValidationStatus)
	}
	return ""
}

func artifactStatusFieldFromIssue(issue connector.Issue, statusField string) (string, bool) {
	statusField = strings.TrimSpace(statusField)
	if statusField == "" {
		statusField = gate.DefaultArtifactStatusField
	}
	for key, value := range issue.Fields {
		if strings.EqualFold(strings.TrimSpace(key), statusField) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

type autoPromoteWorkpadBlockerCheck struct {
	Reason                string
	Resolved              []string
	Source                string
	CommentURL            string
	Invalid               *workpad.Invalid
	Verifications         []AutoPromoteWorkpadBlockerVerification
	ProseFallbackDisabled bool
}

func autoPromoteWorkpadBlocker(issue connector.Issue, cfg AutoPromoteConfig) autoPromoteWorkpadBlockerCheck {
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil {
		return autoPromoteWorkpadBlockerCheck{}
	}
	check := autoPromoteWorkpadBlockerCheck{
		Source:     strings.TrimSpace(signal.Source),
		CommentURL: strings.TrimSpace(signal.CommentURL),
	}
	if signal.Invalid != nil {
		check.Invalid = signal.Invalid
		return check
	}
	if cfg.WorkpadStructuredOnly && signal.Source != workpad.SourceStructured {
		check.ProseFallbackDisabled = true
		return check
	}
	if signal.Source == workpad.SourceStructured {
		return autoPromoteStructuredWorkpadBlockerCheck(issue, signal, cfg.TerminalStates, check)
	}
	reason := workpad.Reason(signal)
	if reason == "" {
		return check
	}
	check.Verifications = autoPromoteTextWorkpadBlockerVerifications(reason, issue, cfg.TerminalStates)
	if resolved, ok := autoPromoteResolvedWorkpadBlockers(reason, issue, cfg.TerminalStates); ok {
		check.Resolved = resolved
		return check
	}
	check.Reason = reason
	return check
}

func autoPromoteTextWorkpadBlockerVerifications(
	reason string,
	issue connector.Issue,
	terminalStates []string,
) []AutoPromoteWorkpadBlockerVerification {
	refs := dependencyRefsInText(reason, dependencyIssueRepo(issue.Identifier))
	if len(refs) == 0 {
		return nil
	}
	blockers := make([]workpad.Blocker, 0, len(refs))
	for _, ref := range refs {
		if identifier := strings.TrimSpace(ref.Identifier); identifier != "" {
			blockers = append(blockers, workpad.Blocker{Identifier: identifier})
		}
	}
	return autoPromoteWorkpadBlockerVerifications(issue, blockers, terminalStates)
}

func autoPromoteStructuredWorkpadBlockerCheck(
	issue connector.Issue,
	signal *workpad.Signal,
	terminalStates []string,
	check autoPromoteWorkpadBlockerCheck,
) autoPromoteWorkpadBlockerCheck {
	check.Verifications = autoPromoteWorkpadBlockerVerifications(issue, signal.Blockers, terminalStates)
	active := false
	for _, verification := range check.Verifications {
		if verification.Status == "resolved" {
			check.Resolved = append(check.Resolved, verification.Identifier)
			continue
		}
		active = true
	}
	if active || strings.TrimSpace(signal.HumanAction) != "" {
		check.Reason = workpad.Reason(signal)
	}
	if active {
		return check
	}
	if strings.TrimSpace(signal.HumanAction) != "" {
		check.Resolved = nil
		return check
	}
	return check
}

func autoPromoteWorkpadBlockerVerifications(
	issue connector.Issue,
	blockers []workpad.Blocker,
	terminalStates []string,
) []AutoPromoteWorkpadBlockerVerification {
	if len(blockers) == 0 {
		return nil
	}
	byIdentifier := make(map[string]connector.BlockedRef, len(issue.BlockedBy))
	for _, ref := range issue.BlockedBy {
		key := strings.ToLower(strings.TrimSpace(ref.Identifier))
		if key != "" {
			byIdentifier[key] = ref
		}
	}
	out := make([]AutoPromoteWorkpadBlockerVerification, 0, len(blockers))
	for _, blocker := range blockers {
		identifier := strings.TrimSpace(blocker.Identifier)
		verification := AutoPromoteWorkpadBlockerVerification{
			Identifier: identifier,
			Status:     "active",
			Reason:     strings.TrimSpace(blocker.Reason),
		}
		if resolved, ok := byIdentifier[strings.ToLower(identifier)]; ok {
			verification.State = strings.TrimSpace(resolved.State)
			if autoPromoteBlockedRefResolved(resolved, terminalStates) {
				verification.Status = "resolved"
			}
		}
		out = append(out, verification)
	}
	return out
}

func autoPromoteApplyWorkpadDecisionFields(decision *AutoPromoteDecision, check autoPromoteWorkpadBlockerCheck) {
	decision.ResolvedWorkpadBlockers = append([]string(nil), check.Resolved...)
	decision.WorkpadCommentURL = strings.TrimSpace(check.CommentURL)
	decision.WorkpadSignalSource = strings.TrimSpace(check.Source)
	decision.WorkpadBlockerVerifications = append([]AutoPromoteWorkpadBlockerVerification(nil), check.Verifications...)
	decision.WorkpadProseFallbackDisabled = check.ProseFallbackDisabled
	if check.Invalid != nil {
		decision.WorkpadStatusInvalid = strings.TrimSpace(check.Invalid.Message)
		decision.WorkpadStatusInvalidHash = strings.TrimSpace(check.Invalid.Hash)
		decision.WorkpadStatusInvalidContent = check.Invalid.Content
	}
}

func autoPromoteResolvedWorkpadBlockers(reason string, issue connector.Issue, terminalStates []string) ([]string, bool) {
	refs := dependencyRefsInText(reason, dependencyIssueRepo(issue.Identifier))
	if len(refs) == 0 {
		return nil, false
	}

	byIdentifier := make(map[string]connector.BlockedRef, len(issue.BlockedBy))
	for _, ref := range issue.BlockedBy {
		key := strings.ToLower(strings.TrimSpace(ref.Identifier))
		if key != "" {
			byIdentifier[key] = ref
		}
	}

	resolved := make([]string, 0, len(refs))
	self := strings.ToLower(strings.TrimSpace(issue.Identifier))
	for _, ref := range refs {
		identifier := strings.TrimSpace(ref.Identifier)
		key := strings.ToLower(identifier)
		if key == "" || key == self {
			continue
		}
		blocker, ok := byIdentifier[key]
		if !ok || !autoPromoteBlockedRefResolved(blocker, terminalStates) {
			return nil, false
		}
		if blocker.Identifier != "" {
			identifier = strings.TrimSpace(blocker.Identifier)
		}
		resolved = append(resolved, identifier)
	}
	if len(resolved) == 0 {
		return nil, false
	}
	return resolved, true
}

func autoPromoteBlockedRefResolved(ref connector.BlockedRef, terminalStates []string) bool {
	state := strings.TrimSpace(ref.State)
	return state != "" && stateIn(state, terminalStates)
}

func autoPromoteIssueWorkpadSignal(issue connector.Issue) (*workpad.Signal, bool) {
	for index := len(issue.Comments) - 1; index >= 0; index-- {
		comment := issue.Comments[index]
		body := comment.Body
		if !autoPromoteIsWorkpadComment(body) {
			continue
		}
		if signal, ok := workpad.SignalFromComment(body, comment.URL, dependencyIssueRepo(issue.Identifier)); ok {
			signal.RecordedAt = autoPromoteWorkpadRecordedAt(comment)
			return signal, true
		}
		if signal := autoPromoteWorkpadProseSignalFromBody(body, comment.URL); signal != nil {
			signal.RecordedAt = autoPromoteWorkpadRecordedAt(comment)
			return signal, true
		}
		return nil, false
	}
	if issue.WorkpadSignal != nil {
		return workpad.CloneSignal(issue.WorkpadSignal), true
	}
	if reason := autoPromoteNormalizeWorkpadBlockerText(issue.BlockerReason); reason != "" {
		return &workpad.Signal{
			Source:      workpad.SourceProse,
			HumanAction: reason,
		}, true
	}
	return nil, false
}

func autoPromoteWorkpadRecordedAt(comment connector.IssueComment) *time.Time {
	if comment.UpdatedAt != nil && !comment.UpdatedAt.IsZero() {
		updatedAt := comment.UpdatedAt.UTC()
		return &updatedAt
	}
	if comment.CreatedAt != nil && !comment.CreatedAt.IsZero() {
		createdAt := comment.CreatedAt.UTC()
		return &createdAt
	}
	return nil
}

func autoPromoteIsWorkpadComment(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		heading, ok := autoPromoteMarkdownHeadingTitle(line)
		if ok && autoPromoteNormalizeSectionTitle(heading) == "codex workpad" {
			return true
		}
	}
	return false
}

func autoPromoteWorkpadProseSignalFromBody(body string, commentURL string) *workpad.Signal {
	sectionFound := false
	for _, title := range []string{"Human Action Needed", "Blockers"} {
		text, ok := autoPromoteMarkdownSectionText(body, title)
		if !ok {
			continue
		}
		sectionFound = true
		if reason := autoPromoteNormalizeWorkpadBlockerText(text); reason != "" {
			return &workpad.Signal{
				Source:      workpad.SourceProseSection,
				CommentURL:  strings.TrimSpace(commentURL),
				HumanAction: reason,
			}
		}
	}
	if sectionFound {
		return nil
	}
	if reason := autoPromoteWorkpadBlockerPhraseReason(body); reason != "" {
		return &workpad.Signal{
			Source:      workpad.SourceProsePhrase,
			CommentURL:  strings.TrimSpace(commentURL),
			HumanAction: reason,
		}
	}
	return nil
}

func autoPromoteWorkpadBlockerPhraseReason(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if _, ok := autoPromoteMarkdownHeadingTitle(line); ok {
			continue
		}
		line = autoPromoteNormalizeWorkpadLine(line)
		if line == "" || autoPromoteWorkpadNonBlockerLine(line) {
			continue
		}
		lower := strings.ToLower(line)
		for _, phrase := range []string{
			"human action needed",
			"human approval",
			"owner approval",
			"owner listening approval",
			"still required",
			"still needed",
			"still needs",
			"required before",
			"waiting for",
			"blocked by:",
			"depends on:",
			"cannot proceed",
			"cannot continue",
		} {
			if strings.Contains(lower, phrase) {
				return line
			}
		}
	}
	return ""
}

func autoPromoteMarkdownSectionText(body string, title string) (string, bool) {
	want := autoPromoteNormalizeSectionTitle(title)
	inSection := false
	found := false
	lines := []string{}
	for line := range strings.SplitSeq(body, "\n") {
		heading, ok := autoPromoteMarkdownHeadingTitle(line)
		if ok {
			if inSection {
				break
			}
			inSection = autoPromoteNormalizeSectionTitle(heading) == want
			if inSection {
				found = true
			}
			continue
		}
		if inSection {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n"), found
}

func autoPromoteMarkdownHeadingTitle(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '#' {
		return "", false
	}
	index := 0
	for index < len(line) && line[index] == '#' {
		index++
	}
	if index > 6 || index == len(line) {
		return "", false
	}
	if line[index] != ' ' && line[index] != '\t' {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(line[index:]), "# \t"), true
}

func autoPromoteNormalizeSectionTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

func autoPromoteNormalizeWorkpadBlockerText(text string) string {
	parts := []string{}
	for line := range strings.SplitSeq(text, "\n") {
		line = autoPromoteNormalizeWorkpadLine(line)
		if line == "" || autoPromoteWorkpadNonBlockerLine(line) {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}

func autoPromoteNormalizeWorkpadLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, ">")
	line = strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if after, ok := strings.CutPrefix(line, marker); ok {
			line = strings.TrimSpace(after)
			break
		}
	}
	for _, marker := range []string{"[ ] ", "[x] ", "[X] "} {
		if after, ok := strings.CutPrefix(line, marker); ok {
			line = strings.TrimSpace(after)
			break
		}
	}
	if index := autoPromoteNumberedListMarkerEnd(line); index > 0 {
		line = strings.TrimSpace(line[index:])
	}
	return strings.Join(strings.Fields(line), " ")
}

func autoPromoteNumberedListMarkerEnd(line string) int {
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index == 0 || index >= len(line) {
		return 0
	}
	if line[index] != '.' && line[index] != ')' {
		return 0
	}
	if index+1 < len(line) && line[index+1] != ' ' && line[index+1] != '\t' {
		return 0
	}
	return index + 1
}

func autoPromoteWorkpadNonBlockerLine(line string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	if autoPromoteResolvedWorkpadLine(line) {
		return true
	}
	if autoPromoteWorkpadNonBlockerSentinel(strings.Trim(line, ".:; ")) {
		return true
	}
	index := strings.IndexAny(line, ".;:")
	if index < 0 {
		return false
	}
	firstClause := strings.Trim(line[:index], ".:; ")
	return firstClause != "" && autoPromoteWorkpadNonBlockerSentinel(firstClause)
}

func autoPromoteResolvedWorkpadLine(line string) bool {
	line = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(line)), " "))
	if line == "" {
		return false
	}
	if strings.Contains(line, "now pass") && (strings.Contains(line, "blocker") || strings.Contains(line, "regression")) {
		return true
	}
	if strings.Contains(line, "removed") && strings.Contains(line, "stale") && strings.Contains(line, "blocked by") {
		return true
	}
	if strings.Contains(line, "blockers are gone") || strings.Contains(line, "blocker is gone") {
		return true
	}
	hasIssueRef := strings.Contains(line, "#") || strings.Contains(line, "/issues/")
	if !hasIssueRef {
		return false
	}
	for _, phrase := range []string{
		"closed/done",
		"already closed",
		"is closed",
		"was closed",
		"merged via",
		"already merged",
		"is merged",
		"was merged",
		"is resolved",
		"was resolved",
		"has been resolved",
		"resolved by",
		"resolved via",
	} {
		if strings.Contains(line, phrase) {
			return true
		}
	}
	return false
}

func autoPromoteWorkpadNonBlockerSentinel(line string) bool {
	switch line {
	case "", "none", "none currently", "n/a", "na", "not applicable", "nothing", "nothing currently",
		"no blockers", "no known blockers", "no unresolved blockers", "no human action needed":
		return true
	default:
		return false
	}
}

func gateFindings(findings []AutoPromoteFinding) []gate.Finding {
	out := make([]gate.Finding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, gate.Finding{
			Body: finding.Body,
			URL:  finding.URL,
			Path: finding.Path,
			Line: finding.Line,
		})
	}
	return out
}

func autoPromoteFindingsFromGate(findings []gate.Finding) []AutoPromoteFinding {
	out := make([]AutoPromoteFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, AutoPromoteFinding{
			Body: finding.Body,
			URL:  finding.URL,
			Path: finding.Path,
			Line: finding.Line,
		})
	}
	return out
}

func autoPromoteActionFromGate(action gate.Action) AutoPromoteAction {
	switch action {
	case gate.ActionPass:
		return AutoPromoteActionPromote
	case gate.ActionRework:
		return AutoPromoteActionRework
	case gate.ActionWait:
		return AutoPromoteActionAwaitReview
	default:
		return AutoPromoteActionSkip
	}
}

func autoPromoteReasonFromGate(reason gate.Reason) AutoPromoteReason {
	switch reason {
	case gate.ReasonReady:
		return AutoPromoteReasonReady
	case gate.ReasonMissingPullRequest:
		return AutoPromoteReasonMissingPullRequest
	case gate.ReasonCINotGreen:
		return AutoPromoteReasonCINotGreen
	case gate.ReasonAutomatedReviewMissing:
		return AutoPromoteReasonCodexReviewMissing
	case gate.ReasonP1Findings:
		return AutoPromoteReasonP1Findings
	case gate.ReasonAutomatedReviewNotQuiet:
		return AutoPromoteReasonCodexReviewNotQuiet
	case gate.ReasonHumanApprovalMissing:
		return AutoPromoteReasonHumanApprovalMissing
	case gate.ReasonValidatorMissing:
		return AutoPromoteReasonValidatorMissing
	case gate.ReasonValidatorError:
		return AutoPromoteReasonValidatorError
	case gate.ReasonValidatorWait:
		return AutoPromoteReasonValidatorWait
	case gate.ReasonValidatorRework:
		return AutoPromoteReasonValidatorRework
	case gate.ReasonValidatorScoreBelowThreshold:
		return AutoPromoteReasonValidatorScoreBelowThreshold
	case gate.ReasonValidatorBlockedSeverity:
		return AutoPromoteReasonValidatorBlockedSeverity
	case gate.ReasonArtifactStatusMissing:
		return AutoPromoteReasonArtifactStatusMissing
	case gate.ReasonArtifactStatusWait:
		return AutoPromoteReasonArtifactStatusWait
	case gate.ReasonArtifactStatusRework:
		return AutoPromoteReasonArtifactStatusRework
	default:
		return AutoPromoteReasonDisabled
	}
}
