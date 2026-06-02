package gate

import (
	"strings"
	"time"
)

const (
	KindCommand     = "command"
	KindHumanReview = "human_review"

	DefaultCommand       = "make check"
	DefaultApprovalLabel = "human-approved"
)

type Config struct {
	Kind          string `yaml:"kind"`
	Run           string `yaml:"run"`
	ApprovalLabel string `yaml:"approval_label"`
}

type Summary struct {
	PullRequestPresent bool
	PullRequestURL     string
	CIStatus           string
	ReviewState        string
	P1Findings         []Finding
	LastActivityAt     *time.Time
}

type Finding struct {
	Body string
	URL  string
	Path string
	Line int
}

type EvaluationOptions struct {
	QuietDuration time.Duration
}

type Action string

const (
	ActionPass   Action = "pass"
	ActionWait   Action = "wait"
	ActionRework Action = "rework"
	ActionSkip   Action = "skip"
)

type Reason string

const (
	ReasonReady                   Reason = "ready"
	ReasonMissingPullRequest      Reason = "missing_pull_request"
	ReasonCINotGreen              Reason = "ci_not_green"
	ReasonAutomatedReviewMissing  Reason = "automated_review_missing"
	ReasonP1Findings              Reason = "p1_findings"
	ReasonAutomatedReviewNotQuiet Reason = "automated_review_not_quiet"
	ReasonHumanApprovalMissing    Reason = "human_approval_missing"
)

type Decision struct {
	Action         Action
	Reason         Reason
	CIStatus       string
	QuietRemaining time.Duration
	Findings       []Finding
}

func DefaultConfig() Config {
	return Config{
		Kind:          KindCommand,
		Run:           DefaultCommand,
		ApprovalLabel: DefaultApprovalLabel,
	}
}

func Effective(cfg Config) Config {
	cfg.Kind = NormalizeKind(cfg.Kind)
	cfg.Run = strings.TrimSpace(cfg.Run)
	cfg.ApprovalLabel = normalizeLabel(cfg.ApprovalLabel)

	if cfg.Kind == "" {
		cfg.Kind = KindCommand
	}
	if cfg.Kind == KindCommand && cfg.Run == "" {
		cfg.Run = DefaultCommand
	}
	if cfg.Kind == KindHumanReview {
		cfg.Run = ""
		if cfg.ApprovalLabel == "" {
			cfg.ApprovalLabel = DefaultApprovalLabel
		}
	}
	if cfg.ApprovalLabel == "" {
		cfg.ApprovalLabel = DefaultApprovalLabel
	}
	return cfg
}

func NormalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", KindCommand:
		return KindCommand
	case KindHumanReview, "human-review", "humanreview":
		return KindHumanReview
	default:
		return strings.ToLower(strings.TrimSpace(kind))
	}
}

func Validate(prefix string, cfg Config) []string {
	switch NormalizeKind(cfg.Kind) {
	case KindCommand, KindHumanReview:
		return nil
	default:
		return []string{prefix + ".kind must be one of command, human_review"}
	}
}

func Instructions(cfg Config) string {
	cfg = Effective(cfg)
	switch cfg.Kind {
	case KindHumanReview:
		return "The validation gate is human review. Keep the pull request in Human Review until a human applies label `" +
			cfg.ApprovalLabel + "`; do not move it to Merging before that label is present."
	default:
		return "The validation gate is a command. Run `" + cfg.Run +
			"` from the workspace root before Human Review and after any rebase in Merging; the pull request still needs green CI and clean automated review before promotion."
	}
}

func Evaluate(cfg Config, labels []string, summary Summary, now time.Time, opts EvaluationOptions) Decision {
	cfg = Effective(cfg)
	switch cfg.Kind {
	case KindHumanReview:
		return evaluateHumanReview(cfg, labels, summary)
	default:
		return evaluateCommand(summary, now, opts)
	}
}

func evaluateCommand(summary Summary, now time.Time, opts EvaluationOptions) Decision {
	if missingPullRequest(summary) {
		return decision(ActionSkip, ReasonMissingPullRequest)
	}
	if normalizedCIStatus(summary.CIStatus) != "green" {
		out := decision(ActionSkip, ReasonCINotGreen)
		out.CIStatus = normalizedCIStatus(summary.CIStatus)
		return out
	}
	if automatedReviewHasP1(summary.ReviewState) || len(summary.P1Findings) > 0 {
		out := decision(ActionRework, ReasonP1Findings)
		out.Findings = cloneFindings(summary.P1Findings)
		return out
	}
	if !automatedReviewSubmitted(summary.ReviewState) {
		return decision(ActionWait, ReasonAutomatedReviewMissing)
	}
	if remaining := quietRemaining(summary, opts, now); remaining > 0 {
		out := decision(ActionWait, ReasonAutomatedReviewNotQuiet)
		out.QuietRemaining = remaining
		return out
	}
	return decision(ActionPass, ReasonReady)
}

func evaluateHumanReview(cfg Config, labels []string, summary Summary) Decision {
	if missingPullRequest(summary) {
		return decision(ActionSkip, ReasonMissingPullRequest)
	}
	for _, label := range labels {
		if normalizeLabel(label) == cfg.ApprovalLabel {
			return decision(ActionPass, ReasonReady)
		}
	}
	return decision(ActionWait, ReasonHumanApprovalMissing)
}

func decision(action Action, reason Reason) Decision {
	return Decision{
		Action: action,
		Reason: reason,
	}
}

func missingPullRequest(summary Summary) bool {
	return !summary.PullRequestPresent && strings.TrimSpace(summary.PullRequestURL) == ""
}

func normalizedCIStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pass", "passed", "success", "successful":
		return "green"
	case "fail", "failed", "failure":
		return "red"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func automatedReviewSubmitted(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED", "COMMENTED", "REQUESTED_CHANGES", "CHANGES_REQUESTED", "P2":
		return true
	default:
		return false
	}
}

func automatedReviewHasP1(state string) bool {
	return strings.ToUpper(strings.TrimSpace(state)) == "P1"
}

func quietRemaining(summary Summary, opts EvaluationOptions, now time.Time) time.Duration {
	if opts.QuietDuration <= 0 {
		return 0
	}
	if summary.LastActivityAt == nil {
		return opts.QuietDuration
	}

	remaining := opts.QuietDuration - now.Sub(*summary.LastActivityAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func cloneFindings(findings []Finding) []Finding {
	return append([]Finding(nil), findings...)
}

func normalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}
