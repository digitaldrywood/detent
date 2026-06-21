package gate

import (
	"strings"
	"time"
)

const (
	KindCommand     = "command"
	KindHumanReview = "human_review"

	DefaultCommand           = "make check"
	DefaultApprovalLabel     = "human-approved"
	DefaultValidatorMinScore = 0.8

	CIFailureActionSkip   = "skip"
	CIFailureActionRework = "rework"
)

type Config struct {
	Kind                   string          `yaml:"kind"`
	Run                    string          `yaml:"run"`
	ApprovalLabel          string          `yaml:"approval_label"`
	RequireAutomatedReview *bool           `yaml:"require_automated_review"`
	CIFailureAction        string          `yaml:"ci_failure_action"`
	Validator              ValidatorConfig `yaml:"validator"`
}

type ValidatorConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Model    string   `yaml:"model"`
	MinScore float64  `yaml:"min_score"`
	BlockOn  []string `yaml:"block_on"`
}

type Summary struct {
	PullRequestPresent bool
	PullRequestURL     string
	CIStatus           string
	ReviewState        string
	P1Findings         []Finding
	Validator          ValidatorResult
	LastActivityAt     *time.Time
}

type Finding struct {
	Severity string
	Body     string
	URL      string
	Path     string
	Line     int
}

type ValidatorResult struct {
	Submitted bool
	Verdict   string
	Score     float64
	Summary   string
	Findings  []Finding
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

const (
	ValidatorVerdictPass   = "pass"
	ValidatorVerdictWait   = "wait"
	ValidatorVerdictRework = "rework"
)

type Reason string

const (
	ReasonReady                        Reason = "ready"
	ReasonMissingPullRequest           Reason = "missing_pull_request"
	ReasonCINotGreen                   Reason = "ci_not_green"
	ReasonAutomatedReviewMissing       Reason = "automated_review_missing"
	ReasonP1Findings                   Reason = "p1_findings"
	ReasonAutomatedReviewNotQuiet      Reason = "automated_review_not_quiet"
	ReasonHumanApprovalMissing         Reason = "human_approval_missing"
	ReasonValidatorMissing             Reason = "validator_missing"
	ReasonValidatorWait                Reason = "validator_wait"
	ReasonValidatorRework              Reason = "validator_rework"
	ReasonValidatorScoreBelowThreshold Reason = "validator_score_below_threshold"
	ReasonValidatorBlockedSeverity     Reason = "validator_blocked_severity"
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
		Kind:                   KindCommand,
		Run:                    DefaultCommand,
		ApprovalLabel:          DefaultApprovalLabel,
		RequireAutomatedReview: new(true),
		CIFailureAction:        CIFailureActionSkip,
		Validator:              effectiveValidatorConfig(ValidatorConfig{}),
	}
}

func Effective(cfg Config) Config {
	cfg.Kind = NormalizeKind(cfg.Kind)
	cfg.Run = strings.TrimSpace(cfg.Run)
	cfg.ApprovalLabel = normalizeLabel(cfg.ApprovalLabel)
	cfg.CIFailureAction = NormalizeCIFailureAction(cfg.CIFailureAction)
	cfg.Validator = effectiveValidatorConfig(cfg.Validator)

	if cfg.Kind == "" {
		cfg.Kind = KindCommand
	}
	if cfg.Kind == KindCommand && cfg.Run == "" {
		cfg.Run = DefaultCommand
	}
	if cfg.Kind == KindHumanReview {
		cfg.Run = ""
		cfg.RequireAutomatedReview = nil
		if cfg.ApprovalLabel == "" {
			cfg.ApprovalLabel = DefaultApprovalLabel
		}
	} else if cfg.RequireAutomatedReview == nil {
		cfg.RequireAutomatedReview = new(true)
	}
	if cfg.ApprovalLabel == "" {
		cfg.ApprovalLabel = DefaultApprovalLabel
	}
	if cfg.CIFailureAction == "" {
		cfg.CIFailureAction = CIFailureActionSkip
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

func NormalizeCIFailureAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", CIFailureActionSkip:
		return CIFailureActionSkip
	case CIFailureActionRework:
		return CIFailureActionRework
	default:
		return strings.ToLower(strings.TrimSpace(action))
	}
}

func Validate(prefix string, cfg Config) []string {
	var problems []string
	switch NormalizeKind(cfg.Kind) {
	case KindCommand, KindHumanReview:
	default:
		problems = append(problems, prefix+".kind must be one of command, human_review")
	}
	switch NormalizeCIFailureAction(cfg.CIFailureAction) {
	case CIFailureActionSkip, CIFailureActionRework:
	default:
		problems = append(problems, prefix+".ci_failure_action must be one of skip, rework")
	}
	problems = append(problems, validateValidator(prefix+".validator", cfg.Validator)...)
	return problems
}

func Instructions(cfg Config) string {
	cfg = Effective(cfg)
	switch cfg.Kind {
	case KindHumanReview:
		return "The validation gate is human review. Keep the pull request in Human Review until a human applies label `" +
			cfg.ApprovalLabel + "`; do not move it to Merging before that label is present."
	default:
		instructions := "The validation gate is a command. Run `" + cfg.Run +
			"` from the workspace root before Human Review; the pull request still needs green CI before promotion. " +
			"In Merging, run a focused rebase/smoke gate after a clean rebase when the PR already passed current-head validation and no source files changed during rebase. " +
			"Run full `" + cfg.Run + "` in Merging when code changes, conflicts are resolved, or validation state is stale or unknown. " +
			"Watch current-head CI with REST check-runs polling/backoff, report slow checks, and record merge wait telemetry in the Workpad: quiet-window wait, local merge-gate duration, PR CI duration, slow check names, and whether post-merge main CI is still running."
		if automatedReviewRequired(cfg) {
			instructions += " Automated review is required on the current pull request head before promotion."
		} else {
			instructions += " Automated review is not required for promotion, but any P1 automated review findings still block promotion."
		}
		if cfg.Validator.Enabled {
			instructions += " A validator-agent review is required before promotion; its structured verdict, score, and configured blocking severities are part of the gate decision."
		}
		return instructions
	}
}

func Evaluate(cfg Config, labels []string, summary Summary, now time.Time, opts EvaluationOptions) Decision {
	cfg = Effective(cfg)
	switch cfg.Kind {
	case KindHumanReview:
		return evaluateHumanReview(cfg, labels, summary)
	default:
		return evaluateCommand(cfg, summary, now, opts)
	}
}

func evaluateCommand(cfg Config, summary Summary, now time.Time, opts EvaluationOptions) Decision {
	if missingPullRequest(summary) {
		return decision(ActionSkip, ReasonMissingPullRequest)
	}
	ciStatus := normalizedCIStatus(summary.CIStatus)
	if ciStatus != "green" {
		out := decision(ciFailureAction(cfg, summary.CIStatus), ReasonCINotGreen)
		out.CIStatus = ciStatus
		return out
	}
	if automatedReviewHasP1(summary.ReviewState) || len(summary.P1Findings) > 0 {
		out := decision(ActionRework, ReasonP1Findings)
		out.Findings = cloneFindings(summary.P1Findings)
		return out
	}
	if automatedReviewRequired(cfg) && !automatedReviewSubmitted(summary.ReviewState) {
		return decision(ActionWait, ReasonAutomatedReviewMissing)
	}
	if remaining := quietRemaining(summary, opts, now); remaining > 0 {
		out := decision(ActionWait, ReasonAutomatedReviewNotQuiet)
		out.QuietRemaining = remaining
		return out
	}
	if out, ok := evaluateValidator(cfg.Validator, summary.Validator); ok {
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

func ciFailureAction(cfg Config, status string) Action {
	if cfg.CIFailureAction == CIFailureActionRework && definitiveCIFailure(status) {
		return ActionRework
	}
	return ActionSkip
}

func definitiveCIFailure(status string) bool {
	switch normalizedCIStatus(status) {
	case "red":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "cancelled", "canceled", "error":
		return true
	default:
		return false
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

func effectiveValidatorConfig(cfg ValidatorConfig) ValidatorConfig {
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.MinScore == 0 {
		cfg.MinScore = DefaultValidatorMinScore
	}
	cfg.BlockOn = normalizeSeverities(cfg.BlockOn)
	if len(cfg.BlockOn) == 0 {
		cfg.BlockOn = []string{"p1"}
	}
	return cfg
}

func validateValidator(prefix string, cfg ValidatorConfig) []string {
	var problems []string
	invalidScore := false
	if cfg.MinScore < 0 || cfg.MinScore > 1 {
		problems = append(problems, prefix+".min_score must be greater than 0 and less than or equal to 1")
		invalidScore = true
	}
	for _, severity := range cfg.BlockOn {
		if strings.TrimSpace(severity) == "" {
			problems = append(problems, prefix+".block_on severities must not be blank")
			break
		}
	}
	cfg = effectiveValidatorConfig(cfg)
	if !invalidScore && (cfg.MinScore <= 0 || cfg.MinScore > 1) {
		problems = append(problems, prefix+".min_score must be greater than 0 and less than or equal to 1")
	}
	return problems
}

func normalizeSeverities(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizeSeverity(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeSeverity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func evaluateValidator(cfg ValidatorConfig, result ValidatorResult) (Decision, bool) {
	cfg = effectiveValidatorConfig(cfg)
	if !cfg.Enabled {
		return Decision{}, false
	}
	if !result.Submitted {
		return decision(ActionWait, ReasonValidatorMissing), true
	}

	findings := cloneFindings(result.Findings)
	if validatorHasBlockedSeverity(cfg, findings) {
		out := decision(ActionRework, ReasonValidatorBlockedSeverity)
		out.Findings = findings
		return out, true
	}

	switch normalizeValidatorVerdict(result.Verdict) {
	case ValidatorVerdictRework:
		out := decision(ActionRework, ReasonValidatorRework)
		out.Findings = findings
		return out, true
	case ValidatorVerdictWait:
		return decision(ActionWait, ReasonValidatorWait), true
	case ValidatorVerdictPass:
	default:
		return decision(ActionWait, ReasonValidatorWait), true
	}

	if result.Score < cfg.MinScore {
		out := decision(ActionRework, ReasonValidatorScoreBelowThreshold)
		out.Findings = findings
		return out, true
	}

	return Decision{}, false
}

func validatorHasBlockedSeverity(cfg ValidatorConfig, findings []Finding) bool {
	blocked := make(map[string]struct{}, len(cfg.BlockOn))
	for _, severity := range cfg.BlockOn {
		blocked[normalizeSeverity(severity)] = struct{}{}
	}
	for _, finding := range findings {
		severity := normalizeSeverity(finding.Severity)
		if severity == "" {
			severity = severityFromFindingBody(finding.Body)
		}
		if _, ok := blocked[severity]; ok {
			return true
		}
	}
	return false
}

func severityFromFindingBody(body string) string {
	upper := strings.ToUpper(body)
	for _, severity := range []string{"P0", "P1", "P2", "P3", "P4"} {
		if strings.Contains(upper, severity) {
			return strings.ToLower(severity)
		}
	}
	return ""
}

func normalizeValidatorVerdict(verdict string) string {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case ValidatorVerdictPass, "passed", "approve", "approved", "ok":
		return ValidatorVerdictPass
	case ValidatorVerdictWait, "waiting", "pending", "inconclusive":
		return ValidatorVerdictWait
	case ValidatorVerdictRework, "fail", "failed", "failure", "request_changes", "requested_changes", "changes_requested", "needs_work":
		return ValidatorVerdictRework
	default:
		return strings.ToLower(strings.TrimSpace(verdict))
	}
}

func normalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

func automatedReviewRequired(cfg Config) bool {
	cfg = Effective(cfg)
	return cfg.Kind == KindCommand && cfg.RequireAutomatedReview != nil && *cfg.RequireAutomatedReview
}
