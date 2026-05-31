package orchestrator

import (
	"strings"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
)

type AutoPromoteConfig struct {
	Enabled            bool
	QuietDuration      time.Duration
	OptoutLabel        string
	AllowedIssueLabels []string
}

type AutoPromoteSummary struct {
	PullRequestPresent bool
	PullRequestURL     string
	CIStatus           string
	ReviewState        string
	P1Findings         []AutoPromoteFinding
	LastActivityAt     *time.Time
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
	AutoPromoteReasonReady               AutoPromoteReason = "ready"
	AutoPromoteReasonDisabled            AutoPromoteReason = "disabled"
	AutoPromoteReasonOptoutLabel         AutoPromoteReason = "optout_label"
	AutoPromoteReasonLabelNotAllowed     AutoPromoteReason = "label_not_allowed"
	AutoPromoteReasonMissingPullRequest  AutoPromoteReason = "missing_pull_request"
	AutoPromoteReasonCINotGreen          AutoPromoteReason = "ci_not_green"
	AutoPromoteReasonCodexReviewMissing  AutoPromoteReason = "codex_review_missing"
	AutoPromoteReasonP1Findings          AutoPromoteReason = "p1_findings"
	AutoPromoteReasonCodexReviewNotQuiet AutoPromoteReason = "codex_review_not_quiet"
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
	if autoPromoteMissingPullRequest(summary) {
		return autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonMissingPullRequest)
	}
	if normalizeAutoPromoteCIStatus(summary.CIStatus) != "green" {
		decision := autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonCINotGreen)
		decision.CIStatus = normalizeAutoPromoteCIStatus(summary.CIStatus)
		return decision
	}
	if !autoPromoteCodexReviewSubmitted(summary) {
		return autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonCodexReviewMissing)
	}
	if len(summary.P1Findings) > 0 {
		decision := autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonP1Findings)
		decision.Findings = cloneAutoPromoteFindings(summary.P1Findings)
		return decision
	}
	if remaining := autoPromoteQuietRemaining(summary, cfg, now); remaining > 0 {
		decision := autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonCodexReviewNotQuiet)
		decision.QuietRemaining = remaining
		return decision
	}

	return autoPromoteDecision(AutoPromoteActionPromote, AutoPromoteReasonReady)
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

func autoPromoteMissingPullRequest(summary AutoPromoteSummary) bool {
	return !summary.PullRequestPresent && strings.TrimSpace(summary.PullRequestURL) == ""
}

func normalizeAutoPromoteCIStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

func autoPromoteCodexReviewSubmitted(summary AutoPromoteSummary) bool {
	switch strings.ToUpper(strings.TrimSpace(summary.ReviewState)) {
	case "APPROVED", "COMMENTED", "REQUESTED_CHANGES", "CHANGES_REQUESTED":
		return true
	default:
		return false
	}
}

func autoPromoteQuietRemaining(summary AutoPromoteSummary, cfg AutoPromoteConfig, now time.Time) time.Duration {
	if cfg.QuietDuration <= 0 {
		return 0
	}
	if summary.LastActivityAt == nil {
		return cfg.QuietDuration
	}

	remaining := cfg.QuietDuration - now.Sub(*summary.LastActivityAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func cloneAutoPromoteFindings(findings []AutoPromoteFinding) []AutoPromoteFinding {
	return append([]AutoPromoteFinding(nil), findings...)
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
