package orchestrator

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

type AutoPromoteConfig struct {
	Enabled            bool
	QuietDuration      time.Duration
	OptoutLabel        string
	AllowedIssueLabels []string
	Gate               gate.Config
}

type AutoPromoteSummary struct {
	PullRequestPresent                    bool
	PullRequestURL                        string
	PullRequestHydrationUnavailableReason string
	PullRequestHydrationDegradedReason    string
	MergeableState                        string
	CIStatus                              string
	ReviewState                           string
	P1Findings                            []AutoPromoteFinding
	Validator                             gate.ValidatorResult
	LastActivityAt                        *time.Time
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
	AutoPromoteReasonPullRequestHydrationUnavailable AutoPromoteReason = "pull_request_hydration_unavailable"
	AutoPromoteReasonMergeConflicts                  AutoPromoteReason = "merge_conflicts"
	AutoPromoteReasonCINotGreen                      AutoPromoteReason = "ci_not_green"
	AutoPromoteReasonCodexReviewMissing              AutoPromoteReason = "automated_review_missing"
	AutoPromoteReasonP1Findings                      AutoPromoteReason = "p1_findings"
	AutoPromoteReasonCodexReviewNotQuiet             AutoPromoteReason = "codex_review_not_quiet"
	AutoPromoteReasonHumanApprovalMissing            AutoPromoteReason = "human_approval_missing"
	AutoPromoteReasonValidatorMissing                AutoPromoteReason = "validator_missing"
	AutoPromoteReasonValidatorWait                   AutoPromoteReason = "validator_wait"
	AutoPromoteReasonValidatorRework                 AutoPromoteReason = "validator_rework"
	AutoPromoteReasonValidatorScoreBelowThreshold    AutoPromoteReason = "validator_score_below_threshold"
	AutoPromoteReasonValidatorBlockedSeverity        AutoPromoteReason = "validator_blocked_severity"
)

type AutoPromoteDecision struct {
	Action         AutoPromoteAction
	Reason         AutoPromoteReason
	CIStatus       string
	QuietRemaining time.Duration
	Findings       []AutoPromoteFinding
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
	if autoPromoteMergeConflicts(summary.MergeableState) {
		return autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonMergeConflicts)
	}
	if strings.TrimSpace(summary.PullRequestHydrationUnavailableReason) != "" ||
		strings.TrimSpace(summary.PullRequestHydrationDegradedReason) != "" {
		return autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonPullRequestHydrationUnavailable)
	}
	gateDecision := gate.Evaluate(cfg.Gate, issue.Labels, gateSummary(summary), now, gate.EvaluationOptions{
		QuietDuration: cfg.QuietDuration,
	})
	decision := autoPromoteDecision(autoPromoteActionFromGate(gateDecision.Action), autoPromoteReasonFromGate(gateDecision.Reason))
	decision.CIStatus = gateDecision.CIStatus
	decision.QuietRemaining = gateDecision.QuietRemaining
	decision.Findings = autoPromoteFindingsFromGate(gateDecision.Findings)
	return decision
}

func autoPromoteDecision(action AutoPromoteAction, reason AutoPromoteReason) AutoPromoteDecision {
	return AutoPromoteDecision{
		Action: action,
		Reason: reason,
	}
}

func normalizeAutoPromoteConfig(cfg AutoPromoteConfig) AutoPromoteConfig {
	if cfg.QuietDuration < 0 {
		cfg.QuietDuration = 0
	}
	cfg.OptoutLabel = normalizeLabel(cfg.OptoutLabel)
	cfg.AllowedIssueLabels = normalizeLabels(cfg.AllowedIssueLabels)
	cfg.Gate = gate.Effective(cfg.Gate)
	return cfg
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
		LastActivityAt:     summary.LastActivityAt,
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
	case gate.ReasonValidatorWait:
		return AutoPromoteReasonValidatorWait
	case gate.ReasonValidatorRework:
		return AutoPromoteReasonValidatorRework
	case gate.ReasonValidatorScoreBelowThreshold:
		return AutoPromoteReasonValidatorScoreBelowThreshold
	case gate.ReasonValidatorBlockedSeverity:
		return AutoPromoteReasonValidatorBlockedSeverity
	default:
		return AutoPromoteReasonDisabled
	}
}
